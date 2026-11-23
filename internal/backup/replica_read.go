package backup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

type memberHello struct {
	Me         string
	SetName    string
	Hosts      []string
	Passives   []string
	Writable   bool `bson:"isWritablePrimary"`
	Secondary  bool
	ElectionID bson.ObjectID
	LastWrite  struct {
		OpTime         struct{ TS bson.Timestamp }
		MajorityOpTime struct{ TS bson.Timestamp }
	}
}

func (s *mongoSource) primaryView() Source {
	return &mongoSource{client: s.client, pref: readpref.Primary()}
}

// Anchor the dump to a single advertised member whose majority snapshot
// includes the primary's last applied write at the start of this database.
func (s *mongoSource) dumpMember(ctx context.Context, uri string) (string, bson.ObjectID, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	primary := mongoSource{client: s.client, pref: readpref.Primary()}
	hello := bson.D{{Key: "hello", Value: 1}}
	var origin memberHello
	if err := primary.command(ctx, hello, &origin); err != nil {
		return "", bson.NilObjectID, err
	}
	barrier := origin.LastWrite.OpTime.TS
	if !origin.Writable || origin.ElectionID.IsZero() || barrier.IsZero() {
		return "", bson.NilObjectID, errors.New("cannot confirm the primary replication position")
	}
	if s.pref.Mode() == readpref.PrimaryMode {
		ready := !origin.LastWrite.MajorityOpTime.TS.Before(barrier)
		if !ready {
			// Respect an explicit or cluster-default majority read concern
			// even when we switch to the writable primary.
			var err error
			ready, err = primary.readsAppliedWrites(ctx, uri)
			if err != nil {
				return "", bson.NilObjectID, err
			}
		}
		if ready {
			pinned, err := directDumpURI(uri, origin.Me, origin.SetName)
			return pinned, origin.ElectionID, err
		}
	}
	advertised := append(origin.Hosts, origin.Passives...)
	if (s.pref.Mode() == readpref.SecondaryMode || s.pref.Mode() == readpref.SecondaryPreferredMode) &&
		len(advertised) == 1 && advertised[0] == origin.Me {
		return "", bson.NilObjectID, errors.New("primary advertises no secondary candidates")
	}
	for {
		var member memberHello
		if err := s.command(ctx, hello, &member); err != nil {
			return "", bson.NilObjectID, err
		}
		if member.Writable && s.pref.Mode() == readpref.SecondaryPreferredMode {
			return "", bson.NilObjectID, errors.New("no secondary was selected")
		}
		if member.SetName != origin.SetName || !slices.Contains(advertised, member.Me) ||
			(!member.Secondary && !(member.Writable && s.pref.Mode() != readpref.SecondaryMode)) {
			return "", bson.NilObjectID, errors.New("cannot verify the selected replica set member for dump reads")
		}
		if !member.LastWrite.OpTime.TS.Before(barrier) && !member.LastWrite.MajorityOpTime.TS.Before(barrier) {
			pinned, err := directDumpURI(uri, member.Me, origin.SetName)
			return pinned, origin.ElectionID, err
		}
		select {
		case <-ctx.Done():
			return "", bson.NilObjectID, fmt.Errorf("selected member did not reach the primary replication position: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Called only for a fallback primary whose majority snapshot is behind.
func (s *mongoSource) readsAppliedWrites(ctx context.Context, uri string) (bool, error) {
	u, err := mongoURIOptions(uri)
	if err != nil {
		return false, err
	}
	for key, values := range u.Query() {
		if strings.EqualFold(key, "readConcernLevel") && len(values) == 1 {
			return values[0] == "local" || values[0] == "available", nil
		}
	}
	var defaults struct{ DefaultReadConcern struct{ Level string } }
	if err := s.command(ctx, bson.D{{Key: "getDefaultRWConcern", Value: 1}}, &defaults); err != nil {
		return false, err
	}
	level := defaults.DefaultReadConcern.Level
	return level == "" || level == "local" || level == "available", nil
}

func directDumpURI(raw, address, set string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(address, "/?#@%") || set == "" {
		return "", errors.New("invalid replica set member address or set name")
	}
	u, err := mongoURIOptions(raw)
	if err != nil {
		return "", err
	}
	query := u.Query()
	hasTLS := false
	for key := range query {
		switch strings.ToLower(key) {
		case "directconnection", "connect", "replicaset", "srvservicename", "srvmaxhosts":
			query.Del(key)
		case "ssl", "tls":
			hasTLS = true
		}
	}
	// Database-scoped authenticated reads already require explicit authSource.
	// Preserve SRV's default TLS and the set identity from the verified primary.
	if u.Scheme == "mongodb+srv" && !hasTLS {
		query.Set("tls", "true")
	}
	query.Set("directConnection", "true")
	query.Set("replicaSet", set)
	u.Scheme, u.Host, u.RawQuery = "mongodb", address, query.Encode()
	return u.String(), nil
}
