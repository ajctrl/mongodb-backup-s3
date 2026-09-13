package backup

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
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
	Inspect(context.Context) (Snapshot, error)
	Databases(context.Context) ([]string, error)
	Close(context.Context) error
}

type Snapshot struct {
	ReplicaSet    json.RawMessage `json:"replica_set"`
	ServerVersion string          `json:"server_version"`
	FCV           string          `json:"feature_compatibility_version"`
}

type mongoSource struct {
	client *mongo.Client
	pref   *readpref.ReadPref
}

// NewSource connects only when the backup caller requests it; restore does not
// need to contact the original server. The backup account needs replSetGetConfig
// and getParameter on the cluster in addition to the privileges used by mongodump.
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
	var hello struct {
		SetName string `bson:"setName"`
		Msg     string `bson:"msg"`
	}
	err = source.command(ctx, bson.D{{Key: "hello", Value: 1}}, &hello)
	if err == nil && (hello.SetName == "" || hello.Msg == "isdbgrid") {
		err = errors.New("MongoDB backup requires a replica set; standalone servers and mongos are not supported")
	}
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
		pem, err := os.ReadFile(c.TLSCA)
		if err != nil {
			return nil, nil, errors.New("cannot read MONGODB_TLS_CA_FILE")
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
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

func (s *mongoSource) Inspect(ctx context.Context) (Snapshot, error) {
	var result Snapshot
	var replica struct {
		Config bson.Raw `bson:"config"`
	}
	if err := s.command(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}, &replica); err != nil {
		return result, err
	}
	if len(replica.Config) == 0 {
		return result, errors.New("replSetGetConfig returned no replica set configuration")
	}
	// Canonical Extended JSON retains BSON types such as ObjectId and int64.
	config, err := bson.MarshalExtJSON(replica.Config, true, false)
	if err != nil {
		return result, errors.New("cannot encode replica set configuration")
	}
	result.ReplicaSet = config
	var build struct {
		Version string `bson:"version"`
	}
	if err := s.command(ctx, bson.D{{Key: "buildInfo", Value: 1}}, &build); err != nil {
		return result, err
	}
	result.ServerVersion = build.Version
	var parameter struct {
		FCV struct {
			Version string `bson:"version"`
		} `bson:"featureCompatibilityVersion"`
	}
	if err := s.command(ctx, bson.D{
		{Key: "getParameter", Value: 1}, {Key: "featureCompatibilityVersion", Value: 1},
	}, &parameter); err != nil {
		return result, err
	}
	result.FCV = parameter.FCV.Version
	if result.ServerVersion == "" || result.FCV == "" {
		return result, errors.New("MongoDB returned incomplete version metadata")
	}
	return result, nil
}

func (s *mongoSource) Databases(ctx context.Context) ([]string, error) {
	var result struct {
		Databases []struct {
			Name string `bson:"name"`
		} `bson:"databases"`
	}
	// authorizedDatabases=false prevents a partial inventory from being mistaken
	// for an all-databases backup when the account has insufficient privileges.
	if err := s.command(ctx, bson.D{
		{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true},
		{Key: "authorizedDatabases", Value: false},
	}, &result); err != nil {
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
			return fmt.Errorf("MongoDB %s denied: backup account lacks required privileges (code 13)", operation)
		}
		return fmt.Errorf("MongoDB %s failed (server error code %d)", operation, commandErr.Code)
	}
	return fmt.Errorf("MongoDB %s failed; check connection, TLS, credentials, and required privileges", operation)
}
