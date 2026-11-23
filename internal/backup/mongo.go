package backup

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

const metadataTimeout = 30 * time.Second

// Source supplies metadata only. Data and authentication records are captured by
// mongodump, so metadata operation timeouts do not limit the duration of a dump.
type Source interface {
	Inspect(context.Context, string) (Snapshot, error)
	primaryView() Source
	dumpMember(context.Context, string) (string, bson.ObjectID, error)
	Databases(ctx context.Context, authorizedOnly bool, name string) ([]string, error)
	Close(context.Context) error
}

type Snapshot struct {
	ExcludedConfigCollections []string        `json:"-"`
	ElectionID                bson.ObjectID   `json:"-"`
	ReplicaSet                json.RawMessage `json:"replica_set"`
	ServerVersion             string          `json:"server_version"`
	FCV                       string          `json:"feature_compatibility_version"`
}

type mongoSource struct {
	client *mongo.Client
	pref   *readpref.ReadPref
}

// NewSource connects to the backup source and verifies the server role with
// getCmdLineOpts before inventory, including for restricted partial backups.
func NewSource(ctx context.Context, c Config) (Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, pref, err := metadataOptions(c)
	if err != nil {
		return nil, err
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, metadataError("initialize MongoDB connection", err)
	}
	source := &mongoSource{client: client, pref: pref}
	// Validate the primary before inventory can select or omit databases.
	primary := mongoSource{client: client, pref: readpref.Primary()}
	_, err = primary.replicaPrimary(ctx)
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		_ = client.Disconnect(closeCtx)
		return nil, err
	}
	return source, nil
}

func metadataOptions(c Config) (*options.ClientOptions, *readpref.ReadPref, error) {
	if c.URI == "" {
		return nil, nil, errors.New("MONGODB_URI is required")
	}
	prefName := c.ReadPreference
	if prefName == "" {
		prefName = "primary"
	}
	mode, err := readpref.ModeFromString(prefName)
	if err != nil {
		return nil, nil, errors.New("invalid MONGODB_READ_PREFERENCE")
	}
	pref, err := readpref.New(mode)
	if err != nil {
		return nil, nil, errors.New("invalid MONGODB_READ_PREFERENCE")
	}
	opts := options.Client().ApplyURI(c.URI).
		SetAppName("mongodb-backup-s3").
		SetConnectTimeout(metadataTimeout).
		SetServerSelectionTimeout(metadataTimeout).
		SetTimeout(metadataTimeout).
		SetReadPreference(pref)
	if err := opts.Validate(); err != nil {
		// The driver may include the original URI in parsing errors.
		return nil, nil, errors.New("invalid MONGODB_URI or MongoDB connection options")
	}
	if c.TLSCA != "" {
		if opts.TLSConfig == nil {
			return nil, nil, errors.New("MONGODB_TLS_CA_FILE requires TLS enabled in MONGODB_URI")
		}
		if opts.TLSConfig.InsecureSkipVerify {
			return nil, nil, errors.New("MONGODB_TLS_CA_FILE requires server certificate and hostname verification")
		}
		// Database Tools understands TLS aliases which the metadata driver may ignore.
		u, err := mongoURIOptions(c.URI)
		if err != nil {
			return nil, nil, err
		}
		for key, values := range u.Query() {
			switch strings.ToLower(key) {
			case "tlscafile", "sslcertificateauthorityfile", "sslcafile":
				if len(values) != 1 || values[0] != c.TLSCA {
					return nil, nil, errors.New("MONGODB_TLS_CA_FILE conflicts with the CA path in MONGODB_URI")
				}
			case "tlsinsecure", "sslinsecure", "tlsallowinvalidcertificates", "tlsallowinvalidhostnames", "sslallowinvalidcertificates", "sslallowinvalidhostnames":
				for _, value := range values {
					if !strings.EqualFold(value, "false") {
						return nil, nil, errors.New("MONGODB_TLS_CA_FILE requires server certificate and hostname verification")
					}
				}
			}
		}
		pem, err := os.ReadFile(c.TLSCA)
		if err != nil {
			return nil, nil, errors.New("cannot read MONGODB_TLS_CA_FILE")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, errors.New("MONGODB_TLS_CA_FILE does not contain a valid PEM certificate")
		}
		tlsConfig := opts.TLSConfig.Clone()
		tlsConfig.RootCAs = pool
		opts.SetTLSConfig(tlsConfig)
	}
	return opts, pref, nil
}

func (s *mongoSource) command(ctx context.Context, command bson.D, result any) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	err := s.client.Database("admin").RunCommand(ctx, command,
		options.RunCmd().SetReadPreference(s.pref)).Decode(result)
	if err != nil {
		return metadataError(command[0].Key, err)
	}
	return nil
}

// Return the election identity only for an ordinary replica set primary.
func (s *mongoSource) replicaPrimary(ctx context.Context) (bson.ObjectID, error) {
	var hello struct {
		SetName    string        `bson:"setName"`
		Msg        string        `bson:"msg"`
		Writable   bool          `bson:"isWritablePrimary"`
		ElectionID bson.ObjectID `bson:"electionId"`
	}
	if err := s.command(ctx, bson.D{{Key: "hello", Value: 1}}, &hello); err != nil {
		return bson.NilObjectID, err
	}
	if hello.SetName == "" || hello.Msg == "isdbgrid" {
		return bson.NilObjectID, errors.New("MongoDB backup requires a replica set; standalone servers and mongos are not supported")
	}
	if !hello.Writable || hello.ElectionID.IsZero() {
		return bson.NilObjectID, errors.New("operation requires a writable replica set primary")
	}
	var cmdline struct {
		Parsed *struct {
			Sharding struct {
				Role string `bson:"clusterRole"`
			} `bson:"sharding"`
		} `bson:"parsed"`
	}
	if err := s.command(ctx, bson.D{{Key: "getCmdLineOpts", Value: 1}}, &cmdline); err != nil {
		return bson.NilObjectID, fmt.Errorf("verify server role (requires the getCmdLineOpts cluster privilege): %w", err)
	}
	if cmdline.Parsed == nil || cmdline.Parsed.Sharding.Role != "" {
		return bson.NilObjectID, errors.New("operation requires a non-sharded replica set; cannot verify a supported server role")
	}
	return hello.ElectionID, nil
}

func (s *mongoSource) Inspect(ctx context.Context, mode string) (Snapshot, error) {
	partial := mode == "partial"
	result, err := s.versions(ctx, partial)
	if err != nil {
		return result, err
	}
	primary := mongoSource{client: s.client, pref: readpref.Primary()}
	result.ElectionID, err = primary.replicaPrimary(ctx)
	if err != nil {
		return result, err
	}
	if mode == "full" {
		result.ExcludedConfigCollections, err = s.excludedConfigCollections(ctx)
		if err != nil {
			return result, err
		}
	}
	var replica struct {
		Config bson.Raw `bson:"config"`
	}
	if err := s.command(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}, &replica); err != nil {
		if partial && errors.Is(err, errUnauthorized) {
			return result, nil
		}
		return result, err
	}
	if len(replica.Config) == 0 {
		return result, errors.New("replSetGetConfig returned no replica set configuration")
	}
	result.ReplicaSet, err = bson.MarshalExtJSON(replica.Config, true, false)
	if err != nil {
		return result, errors.New("cannot encode replica set configuration")
	}
	return result, nil
}

// Database Tools 100.20.1 keeps seven config collections in cluster dumps
// (common/dumprestore/config.go). Recognize internal names of ordinary replica
// sets explicitly; an unknown name is always disclosed, including across upgrades.
func (s *mongoSource) excludedConfigCollections(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	names, err := s.client.Database("config").ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, metadataError("list config collections", err)
	}
	var excluded []string
	for _, name := range names {
		switch name {
		case "chunks", "collections", "databases", "settings", "shards", "tags", "version":
		case "system.sessions", "transactions", "image_collection", "system.preimages", "system.indexBuilds",
			"system.change_collection", "system.profile", "system.views", "clusterParameters", "external_validation_keys",
			"user_writes_critical_sections", "sampledQueries", "sampledQueriesDiff", "queryAnalyzers", "analyzeShardKeySplitPoints":
		default:
			excluded = append(excluded, name)
		}
	}
	return excluded, nil
}

func targetVersions(ctx context.Context, c Config) (Snapshot, error) {
	c.ReadPreference = "primary"
	opts, pref, err := metadataOptions(c)
	if err != nil {
		return Snapshot{}, err
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return Snapshot{}, metadataError("initialize restore connection", err)
	}
	s := mongoSource{client: client, pref: pref}
	defer s.Close(context.Background())
	if c.BackupMode == "full" {
		if _, err := s.replicaPrimary(ctx); err != nil {
			return Snapshot{}, fmt.Errorf("verify full restore target: %w", err)
		}
	}
	if c.BackupMode == "full" && !c.AllowNonemptyFullRestore {
		names, err := s.Databases(ctx, false, "")
		if err != nil {
			return Snapshot{}, fmt.Errorf("cannot verify that the restore target is empty: %w", err)
		}
		for _, name := range names {
			if !systemDatabase(name) {
				return Snapshot{}, fmt.Errorf("target contains database %q; full oplog replay can delete existing data and requires MONGODB_RESTORE_ALLOW_NONEMPTY_TARGET=true", name)
			}
		}
	}
	return s.versions(ctx, false)
}

func (s *mongoSource) versions(ctx context.Context, partial bool) (Snapshot, error) {
	var result Snapshot
	var build struct {
		Version string `bson:"version"`
	}
	if err := s.command(ctx, bson.D{{Key: "buildInfo", Value: 1}}, &build); err != nil {
		if !partial || !errors.Is(err, errUnauthorized) {
			return result, err
		}
	} else if build.Version == "" {
		return result, errors.New("MongoDB returned no server version")
	}
	result.ServerVersion = build.Version
	var parameter struct {
		FCV struct {
			Version         string  `bson:"version"`
			TargetVersion   *string `bson:"targetVersion"`
			PreviousVersion *string `bson:"previousVersion"`
		} `bson:"featureCompatibilityVersion"`
	}
	if err := s.command(ctx, bson.D{
		{Key: "getParameter", Value: 1}, {Key: "featureCompatibilityVersion", Value: 1},
	}, &parameter); err != nil {
		if partial && errors.Is(err, errUnauthorized) {
			return result, nil
		}
		return result, err
	}
	if parameter.FCV.TargetVersion != nil || parameter.FCV.PreviousVersion != nil {
		return result, errors.New("MongoDB feature compatibility version transition in progress")
	}
	result.FCV = parameter.FCV.Version
	if result.FCV == "" {
		return result, errors.New("MongoDB returned incomplete version metadata")
	}
	return result, nil
}

func (s *mongoSource) Databases(ctx context.Context, authorizedOnly bool, name string) ([]string, error) {
	var result struct {
		Databases []struct {
			Name string `bson:"name"`
		} `bson:"databases"`
	}
	// authorizedDatabases=false prevents a partial inventory from being mistaken
	// for an all-databases backup when the account has insufficient privileges.
	cmd := bson.D{
		{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true},
		{Key: "authorizedDatabases", Value: authorizedOnly},
	}
	if name != "" {
		cmd = append(cmd, bson.E{Key: "filter", Value: bson.D{{Key: "name", Value: name}}})
	}
	// Tools enumerate collections on the primary even for secondary data reads.
	primary := mongoSource{client: s.client, pref: readpref.Primary()}
	if err := primary.command(ctx, cmd, &result); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Databases))
	for _, db := range result.Databases {
		names = append(names, db.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (s *mongoSource) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	if err := s.client.Disconnect(ctx); err != nil {
		return metadataError("close MongoDB connection", err)
	}
	return nil
}

var errUnauthorized = errors.New("backup account lacks required privileges (code 13)")

// Never include driver error text: URI, credentials, certificate contents, or
// server-controlled response strings must not reach backup logs.
func metadataError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("MongoDB %s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) || mongo.IsTimeout(err) {
		return fmt.Errorf("MongoDB %s: %w", operation, context.DeadlineExceeded)
	}
	var commandErr mongo.CommandError
	if errors.As(err, &commandErr) {
		if commandErr.Code == 13 {
			return fmt.Errorf("MongoDB %s denied: %w", operation, errUnauthorized)
		}
		return fmt.Errorf("MongoDB %s failed (server error code %d)", operation, commandErr.Code)
	}
	return fmt.Errorf("MongoDB %s failed; check connection, TLS, credentials, and required privileges", operation)
}
