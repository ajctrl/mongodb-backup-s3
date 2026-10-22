//go:build integration

package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// Both the Go metadata commands and MongoDB Tools' data reads must use the secondary.
// The TLS case also exercises the final image's CA handling for both clients.
func TestSecondaryBackupRestore(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", useTLS), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
			defer cancel()
			f := newFixtureWithOptions(t, ctx, fixtureOptions{tls: useTLS, secondary: true})
			source := f.connect(t, f.sourceURI)
			f.seed(t, source)
			secondary := f.connect(t, authenticatedURI(f.secondaryPort, "source"))
			payload := secondary.Database("app", options.Database().SetReadPreference(readpref.Secondary())).Collection("payload")
			waitFor(t, ctx, "secondary replication", func(ctx context.Context) error {
				count, err := payload.CountDocuments(ctx, bson.M{})
				if err != nil {
					return err
				}
				if count != 1024 {
					return fmt.Errorf("replicated payload count=%d", count)
				}
				return nil
			})
			for _, mode := range []string{"full", "per-database"} {
				if !t.Run(mode, func(t *testing.T) {
					env := map[string]string{
						"S3_PREFIX": "secondary-" + mode, "BACKUP_MODE": mode,
						"BACKUP_FILENAME_MODE": "fixed", "MONGODB_READ_PREFERENCE": "secondary",
					}
					if mode == "per-database" {
						env["MONGODB_DATABASE"] = "app"
					}
					// Fail data and replica metadata reads on the primary. MongoDB Tools
					// still use the primary for database/collection enumeration.
					func() {
						f.command(t, source.Database("admin"), bson.D{
							{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "alwaysOn"},
							{Key: "data", Value: bson.M{"failCommands": bson.A{"find", "replSetGetConfig", "getParameter"}, "errorCode": 2}},
						})
						defer f.command(t, source.Database("admin"), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}})
						f.run(t, env, "backup")
					}()
					keys := f.keys(t, "secondary-"+mode+"/")
					if len(keys) != 1 {
						t.Fatalf("secondary backup published %v", keys)
					}
					f.assertBundle(t, keys[0], mode == "per-database")
					env["MONGODB_URI"] = f.targetURI
					env["MONGODB_RESTORE_DROP"] = "true"
					// Keep secondary selected: restores must still write to the
					// single-member primary target, including with a custom TLS CA.
					f.run(t, env, "restore")
					target := f.connect(t, f.targetURI)
					f.assertValue(t, target, "app", "original")
					f.assertIndex(t, target)
					if mode == "full" {
						f.assertValue(t, target, "analytics", "analytics original")
						f.assertAuth(t, "app", "appreader")
					}
				}) {
					return
				}
			}
			if useTLS {
				f.checkTLSRejections(t)
			}
		})
	}
}

func (f *fixture) checkTLSRejections(t *testing.T) {
	t.Helper()
	for _, reason := range []string{"wrong CA", "wrong hostname"} {
		t.Run(reason, func(t *testing.T) {
			env := map[string]string{
				"BACKUP_MODE": "per-database", "MONGODB_DATABASE": "app", "BACKUP_FILENAME_MODE": "fixed",
				"S3_PREFIX": "rejected-" + strings.ReplaceAll(reason, " ", "-"),
			}
			badURI := func(uri string) string {
				// Direct mode prevents discovery of a different, valid hostname.
				uri += "&directConnection=true"
				if reason == "wrong hostname" {
					uri = strings.Replace(uri, "127.0.0.1", "localhost", 1)
				}
				return uri
			}
			env["MONGODB_URI"] = badURI(f.baseEnv["MONGODB_URI"])
			if reason == "wrong CA" {
				env["MONGODB_TLS_CA_FILE"] = filepath.Join(filepath.Dir(f.baseEnv["MONGODB_TLS_CA_FILE"]), "wrong-ca.pem")
			}
			output, err := f.runCommand(t, env, "backup")
			if err == nil || !strings.Contains(output, "MongoDB hello") {
				t.Fatalf("metadata accepted %s or failed elsewhere: error=%v output=%s", reason, err, output)
			}
			if keys := f.keys(t, env["S3_PREFIX"]+"/"); len(keys) != 0 {
				t.Fatalf("invalid TLS connection published %v", keys)
			}
			// Restore bypasses the Go metadata client. Test the tool's certificate
			// checks independently, using the previously validated saved bundle.
			target := f.connect(t, f.targetURI)
			f.setValue(t, target, "app", "unchanged after TLS failure")
			env["S3_PREFIX"] = "secondary-per-database"
			env["MONGODB_URI"] = badURI(f.targetURI)
			env["MONGODB_RESTORE_DROP"] = "true"
			output, err = f.runCommand(t, env, "restore")
			if err == nil || !strings.Contains(output, "mongorestore failed") || !strings.Contains(strings.ToLower(output), "certificate") {
				t.Fatalf("mongorestore accepted %s or failed elsewhere: error=%v output=%s", reason, err, output)
			}
			f.assertValue(t, target, "app", "unchanged after TLS failure")
		})
	}
}

func (f *fixture) mongoTLSArgs(dir string) []string {
	if f.tlsConfig == nil {
		return nil
	}
	return []string{"--tlsMode", "requireTLS", "--tlsCertificateKeyFile", filepath.Join(dir, "server.pem"),
		"--tlsCAFile", filepath.Join(dir, "ca.pem"), "--tlsAllowConnectionsWithoutCertificates"}
}

func (f *fixture) copyTLSCommand() string {
	if f.tlsConfig == nil {
		return ""
	}
	// The official entrypoint drops root privileges. Copy the test-only private
	// key from the host's private fixture directory before that transition.
	return "cp /test-tls/server.pem /tmp/server.pem; cp /test-tls/ca.pem /tmp/ca.pem; chmod 600 /tmp/server.pem; chown mongodb:mongodb /tmp/server.pem; "
}

func (f *fixture) addSecondary(t *testing.T) {
	t.Helper()
	if f.native {
		dir := t.TempDir()
		key := filepath.Join(dir, "keyFile")
		if err := os.WriteFile(key, []byte(testReplicaKey), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--dbpath", dir, "--bind_ip", "127.0.0.1", "--port", f.secondaryPort,
			"--replSet", "source", "--keyFile", key, "--oplogSize", "128"}
		startProcess(t, requireBinary(t, "MONGOD_TEST_BINARY", "mongod"), nil, append(args, f.mongoTLSArgs(f.configDir)...)...)
	} else {
		name := f.name + "-secondary"
		t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", name) })
		args := []string{"run", "-d", "--name", name, "--network", "container:" + f.anchor}
		if f.tlsConfig != nil {
			args = append(args, "--mount", "type=bind,source="+f.configDir+",target=/test-tls,readonly")
		}
		args = append(args, "--entrypoint", "bash", envOr("MONGODB_TEST_IMAGE", "mongo:8.0"), "-c",
			"set -eu; printf '%s' "+testReplicaKey+" >/tmp/test-key; chmod 600 /tmp/test-key; chown mongodb:mongodb /tmp/test-key; "+f.copyTLSCommand()+
				"exec docker-entrypoint.sh mongod --bind_ip_all --port "+f.secondaryPort+" --replSet source --keyFile /tmp/test-key --oplogSize 128 "+strings.Join(f.mongoTLSArgs("/tmp"), " "))
		docker(t, f.ctx, args...)
	}
	unauthenticated := f.connect(t, "mongodb://127.0.0.1:"+f.secondaryPort+"/?directConnection=true")
	waitFor(t, f.ctx, "secondary startup", func(ctx context.Context) error {
		return unauthenticated.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Err()
	})
	admin := f.connect(t, f.sourceURI).Database("admin")
	var current struct{ Config bson.M }
	if err := admin.RunCommand(f.ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&current); err != nil {
		t.Fatal(err)
	}
	version, ok := current.Config["version"].(int32)
	if !ok {
		t.Fatalf("unexpected replica set version: %v", current.Config["version"])
	}
	current.Config["version"] = version + 1
	current.Config["members"] = append(current.Config["members"].(bson.A), bson.M{
		"_id": 1, "host": "127.0.0.1:" + f.secondaryPort, "priority": 0, "votes": 0,
	})
	f.command(t, admin, bson.D{{Key: "replSetReconfig", Value: current.Config}})
	secondary := f.connect(t, authenticatedURI(f.secondaryPort, "source"))
	waitFor(t, f.ctx, "authenticated secondary", func(ctx context.Context) error {
		var hello struct{ Secondary bool }
		if err := secondary.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
			return err
		}
		if !hello.Secondary {
			return fmt.Errorf("secondary still synchronizing")
		}
		return nil
	})
}

// Generate fresh, short-lived test certificates without requiring openssl on
// the CI host. Only 127.0.0.1 is a SAN, so localhost is a negative hostname case.
func createTestCertificates(t *testing.T, dir string) *tls.Config {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	serial := func() *big.Int {
		n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			t.Fatal(err)
		}
		return n.Add(n, big.NewInt(1))
	}
	create := func(template, parent *x509.Certificate, key, signer *ecdsa.PrivateKey) []byte {
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	write := func(name string, content []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), content, mode); err != nil {
			t.Fatal(err)
		}
	}
	caKey := newKey()
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Integration CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: create(ca, ca, caKey, caKey)})
	write("ca.pem", caPEM, 0644)
	wrongKey := newKey()
	wrongCA := *ca
	wrongCA.SerialNumber = serial()
	wrongCA.Subject.CommonName = "Untrusted integration CA"
	write("wrong-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: create(&wrongCA, &wrongCA, wrongKey, wrongKey)}), 0644)
	serverKey := newKey()
	server := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Integration MongoDB"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	serverPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: create(server, ca, serverKey, caKey)})
	private, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverPEM = append(serverPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})...)
	write("server.pem", serverPEM, 0600)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("invalid generated CA")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}
