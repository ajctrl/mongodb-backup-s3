package backup

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajctrl/mongodb-backup-s3/internal/storage"
)

const timestampLayout = "2006-01-02T15:04:05"

var generationPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}-[0-9a-f]{32}$`)

func validGeneration(name string) bool {
	if !generationPattern.MatchString(name) {
		return false
	}
	_, err := time.Parse(timestampLayout, name[:len(timestampLayout)])
	return err == nil
}

type Config struct {
	URI, TLSCA, ReadPreference, ConfigDir                string
	Database, BackupAll, BackupMode                      string
	Databases, Exclude                                   []string
	Prefix, Passphrase, KeepDays, FilenameMode, Schedule string
	RestoreDrop                                          bool
	ParallelCollections                                  int
	Storage                                              storage.Config
}

func LoadConfig() (Config, error) { return loadConfig(os.LookupEnv) }

func loadConfig(lookup func(string) (string, bool)) (Config, error) {
	get := func(key string) string { value, _ := lookup(key); return value }
	valueOr := func(key, fallback string) string {
		if v := get(key); v != "" {
			return v
		}
		return fallback
	}
	prefix, present := lookup("S3_PREFIX")
	if !present {
		prefix = "backup"
	}
	c := Config{
		URI: get("MONGODB_URI"), TLSCA: get("MONGODB_TLS_CA_FILE"), ReadPreference: valueOr("MONGODB_READ_PREFERENCE", "primary"), ConfigDir: get("MONGODB_CONFIG_DIR"),
		Database: get("MONGODB_DATABASE"), BackupAll: valueOr("MONGODB_BACKUP_ALL", "false"), BackupMode: valueOr("BACKUP_MODE", "full"),
		Prefix: prefix, Passphrase: get("PASSPHRASE"), KeepDays: get("BACKUP_KEEP_DAYS"), FilenameMode: valueOr("BACKUP_FILENAME_MODE", "timestamp"), Schedule: get("SCHEDULE"),
		Storage: storage.Config{Bucket: get("S3_BUCKET"), Region: valueOr("S3_REGION", "us-west-1"), Endpoint: get("S3_ENDPOINT"), AccessKeyID: get("S3_ACCESS_KEY_ID"), SecretAccessKey: get("S3_SECRET_ACCESS_KEY"), SessionToken: get("S3_SESSION_TOKEN")},
	}
	for _, list := range []struct {
		name string
		dest *[]string
	}{{"MONGODB_DATABASES", &c.Databases}, {"MONGODB_DATABASES_EXCLUDE", &c.Exclude}} {
		names, err := parseDatabaseList(get(list.name))
		if err != nil {
			return c, fmt.Errorf("%s: %w", list.name, err)
		}
		*list.dest = names
	}
	if p := get("MONGODB_URI_FILE"); p != "" {
		if c.URI != "" {
			return c, fmt.Errorf("set only one of MONGODB_URI and MONGODB_URI_FILE")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return c, fmt.Errorf("read MONGODB_URI_FILE: %w", err)
		}
		c.URI = strings.TrimRight(string(b), "\r\n")
	}
	if c.Storage.Bucket == "" {
		return c, fmt.Errorf("set S3_BUCKET")
	}
	if c.URI != "" {
		if err := validateURI(c.URI); err != nil {
			return c, err
		}
	}
	if c.BackupMode != "full" && c.BackupMode != "per-database" {
		return c, fmt.Errorf("BACKUP_MODE must be full or per-database")
	}
	if c.BackupAll != "true" && c.BackupAll != "false" {
		return c, fmt.Errorf("MONGODB_BACKUP_ALL must be true or false")
	}
	if c.FilenameMode != "timestamp" && c.FilenameMode != "fixed" {
		return c, fmt.Errorf("BACKUP_FILENAME_MODE must be timestamp or fixed")
	}
	if strings.ContainsAny(c.Passphrase, "\r\n") {
		return c, fmt.Errorf("PASSPHRASE must not contain newlines")
	}
	switch c.ReadPreference {
	case "primary", "primaryPreferred", "secondary", "secondaryPreferred", "nearest":
	default:
		return c, fmt.Errorf("invalid MONGODB_READ_PREFERENCE")
	}
	drop := valueOr("MONGODB_RESTORE_DROP", "false")
	if drop != "true" && drop != "false" {
		return c, fmt.Errorf("MONGODB_RESTORE_DROP must be true or false")
	}
	c.RestoreDrop = drop == "true"
	n, err := strconv.Atoi(valueOr("MONGODB_PARALLEL_COLLECTIONS", "4"))
	if err != nil || n < 1 || n > 128 {
		return c, fmt.Errorf("MONGODB_PARALLEL_COLLECTIONS must be between 1 and 128")
	}
	c.ParallelCollections = n
	n, err = strconv.Atoi(valueOr("S3_UPLOAD_PART_SIZE_MB", "8"))
	if err != nil || n < 5 || n > 5120 {
		return c, fmt.Errorf("S3_UPLOAD_PART_SIZE_MB must be between 5 and 5120 MiB")
	}
	c.Storage.UploadPartSizeBytes = int64(n) * 1024 * 1024
	return c, nil
}

// URI database paths change mongodump's selection, so reject them even in DB mode.
// Never return a URL parser error: it can contain passwords.
func validateURI(raw string) error {
	u, err := mongoURIOptions(raw)
	if err != nil {
		return fmt.Errorf("invalid MONGODB_URI")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("MONGODB_URI must omit the database path; set authSource in its query")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid MONGODB_URI query")
	}
	for key := range q {
		switch strings.ToLower(key) {
		case "readpreference", "readpreferencetags", "maxstalenessseconds":
			return fmt.Errorf("use MONGODB_READ_PREFERENCE instead of read preference URI options")
		}
	}
	return nil
}

// net/url assumes one host and rejects valid MongoDB seed lists containing
// several ports. Parse credentials/path/query using a placeholder authority;
// the MongoDB driver and Database Tools validate the actual seed list.
func mongoURIOptions(raw string) (*url.URL, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || (scheme != "mongodb" && scheme != "mongodb+srv") || strings.ContainsAny(raw, " \t\r\n\x00#") {
		return nil, fmt.Errorf("invalid MONGODB_URI")
	}
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	authority, suffix := rest[:end], rest[end:]
	userinfo, hosts := "", authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		userinfo, hosts = authority[:at+1], authority[at+1:]
	}
	for _, host := range strings.Split(hosts, ",") {
		if host == "" {
			return nil, fmt.Errorf("invalid MONGODB_URI")
		}
	}
	u, err := url.Parse(scheme + "://" + userinfo + "mongodb.invalid" + suffix)
	if err != nil {
		return nil, fmt.Errorf("invalid MONGODB_URI")
	}
	return u, nil
}

func (c Config) requireConnection() error {
	if c.URI == "" {
		return fmt.Errorf("set MONGODB_URI or MONGODB_URI_FILE")
	}
	return validateURI(c.URI)
}

func (c Config) validateBackup() (int, error) {
	if err := c.requireConnection(); err != nil {
		return 0, err
	}
	if c.BackupMode == "full" {
		if c.Database != "" || len(c.Databases) > 0 || len(c.Exclude) > 0 || c.BackupAll == "true" {
			return 0, fmt.Errorf("full backup requires clearing all MongoDB database selectors")
		}
	} else if c.BackupMode == "per-database" {
		if c.BackupAll == "true" && (c.Database != "" || len(c.Databases) > 0) {
			return 0, fmt.Errorf("MONGODB_BACKUP_ALL cannot be combined with explicit databases")
		}
		if c.BackupAll != "true" && c.Database == "" && len(c.Databases) == 0 {
			return 0, fmt.Errorf("set MONGODB_DATABASE, MONGODB_DATABASES or MONGODB_BACKUP_ALL=true")
		}
		if len(c.Exclude) > 0 && c.BackupAll != "true" {
			return 0, fmt.Errorf("MONGODB_DATABASES_EXCLUDE requires MONGODB_BACKUP_ALL=true")
		}
		if len(c.Databases) > 0 {
			if err := validateDatabases(c.Databases); err != nil {
				return 0, err
			}
		} else if c.Database != "" {
			if err := validateDatabases([]string{c.Database}); err != nil {
				return 0, err
			}
		}
		for _, name := range c.Exclude {
			if err := validateDatabaseName(name); err != nil {
				return 0, err
			}
		}
	} else {
		return 0, fmt.Errorf("BACKUP_MODE must be full or per-database")
	}
	if c.KeepDays == "" {
		return 0, nil
	}
	if c.KeepDays[0] == '0' || strings.IndexFunc(c.KeepDays, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("BACKUP_KEEP_DAYS must be a positive integer without leading zeros")
	}
	n, err := strconv.Atoi(c.KeepDays)
	if err != nil || n < 1 || n > 36500 {
		return 0, fmt.Errorf("BACKUP_KEEP_DAYS must be between 1 and 36500")
	}
	return n, nil
}

func parseDatabaseList(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var names []string
	// Require an array, not JSON null. Do not trim individual names: whitespace
	// may be part of a Linux DB name, and separators may be literal commas.
	if !utf8.ValidString(value) || !strings.HasPrefix(strings.TrimSpace(value), "[") || json.Unmarshal([]byte(value), &names) != nil {
		return nil, fmt.Errorf("must be a JSON array of database names (for example [\"app\",\"private,archive\"]); comma-separated lists are no longer supported")
	}
	for _, name := range names {
		if err := validateDatabaseName(name); err != nil {
			return nil, err
		}
	}
	return names, nil
}
func systemDatabase(name string) bool { return name == "admin" || name == "local" || name == "config" }
func validateDatabases(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("no databases selected")
	}
	for _, name := range names {
		if err := validateDatabaseName(name); err != nil {
			return err
		}
		if systemDatabase(name) {
			return fmt.Errorf("internal databases require full mode; authentication is included separately in per-database bundles")
		}
	}
	return nil
}
func validateDatabaseName(name string) error {
	// This application runs on Linux; Windows-only restrictions do not apply.
	if name == "" || len(name) >= 64 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\.\"$ \x00") {
		return fmt.Errorf("invalid MongoDB database name %q", name)
	}
	return nil
}
func encodeComponent(value string) string {
	const hex = "0123456789ABCDEF"
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b)) {
			result.WriteByte(b)
		} else {
			result.WriteByte('%')
			result.WriteByte(hex[b>>4])
			result.WriteByte(hex[b&15])
		}
	}
	return result.String()
}
func (c Config) fileType() string {
	if c.Passphrase != "" {
		return ".zip.gpg"
	}
	return ".zip"
}
func (c Config) timestampPrefix(database string) string {
	p := strings.TrimRight(c.Prefix, "/")
	if p != "" {
		p += "/"
	}
	if c.BackupMode == "full" {
		return p + "full/"
	}
	return p + "per-database/" + encodeComponent(database) + "/"
}
func (c Config) fixedKey(database string) string {
	return c.timestampPrefix(database) + "latest" + c.fileType()
}
func (c Config) backupKey(database string, now time.Time) string {
	if c.FilenameMode == "fixed" {
		return c.fixedKey(database)
	}
	var id [16]byte
	rand.Read(id[:]) // crypto/rand.Read always fills the buffer or terminates the process.
	return c.timestampPrefix(database) + now.UTC().Format(timestampLayout) + fmt.Sprintf("-%x", id) + c.fileType()
}
func (c Config) isTimestampBackup(database, key string) bool {
	prefix := c.timestampPrefix(database)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".gpg")
	return strings.HasSuffix(name, ".zip") && validGeneration(strings.TrimSuffix(name, ".zip"))
}
