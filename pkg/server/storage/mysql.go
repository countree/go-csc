package storage

import (
	"database/sql"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"bitbucket.org/liamstask/goose/lib/goose"
	"github.com/go-sql-driver/mysql"
	"github.com/square/sharkey/pkg/server/config"
)

// MysqlStorage implements the storage interface, using Mysql for storage.
type MysqlStorage struct {
	*sql.DB
}

const (
	mysqlHostCert = "host_cert"
	mysqlUserCert = "user_cert"
)

var _ Storage = &MysqlStorage{}

func (my *MysqlStorage) RecordIssuance(certType uint32, principal string, pubkey ssh.PublicKey) (uint64, error) {
	pkdata := ssh.MarshalAuthorizedKey(pubkey)

	typ, err := certTypeToMySQL(certType)
	if err != nil {
		return 0, err
	}

	result, err := my.DB.Exec(
		"INSERT INTO hostkeys (hostname, pubkey, cert_type) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE pubkey = ?",
		principal, pkdata, typ, pkdata)
	if err != nil {
		return 0, fmt.Errorf("error recording issuance: %s", err.Error())

	}

	// TODO: This is broken!  It doesn't work in the ON DUPLICATE KEY case.
	//       Tracked in https://github.com/square/sharkey/issues/80
	id, err := result.LastInsertId()
	return uint64(id), err
}

func (my *MysqlStorage) QueryHostkeys() (ResultIterator, error) {
	rows, err := my.DB.Query("SELECT hostname, pubkey FROM hostkeys WHERE cert_type = ?",
		mysqlHostCert)
	if err != nil {
		return &SqlResultIterator{}, err
	}
	return &SqlResultIterator{Rows: rows}, nil
}

func (my *MysqlStorage) RecordGitHubMapping(mapping map[string]string) error {
	// Prepare for batch insert
	insertEntries := make([]string, 0, len(mapping))
	insertValues := make([]interface{}, 0, len(mapping)*2)
	deleteEntries := make([]string, 0, len(mapping))
	deleteValues := make([]interface{}, 0, len(mapping))
	for ssoIdentity, githubUser := range mapping {
		// Create one set of values for each mapping
		insertEntries = append(insertEntries, "(?, ?)")
		// Append matching values for mapping
		insertValues = append(insertValues, ssoIdentity)
		insertValues = append(insertValues, githubUser)

		deleteEntries = append(deleteEntries, "?")
		deleteValues = append(deleteValues, ssoIdentity)
	}

	// Delete if not found in GitHub results
	deleteStmt := fmt.Sprintf(
		"DELETE FROM github_user_mappings WHERE sso_identity NOT IN (%s)",
		strings.Join(deleteEntries, ","))
	_, err := my.DB.Exec(deleteStmt, deleteValues...)
	if err != nil {
		return fmt.Errorf("error deleting mappings: %s", err.Error())
	}

	insertStmt := fmt.Sprintf(
		"REPLACE INTO github_user_mappings (sso_identity, github_username) VALUES %s",
		strings.Join(insertEntries, ","))
	// Execute with blown up values that match into the (?, ?) blocks inserted into the statement
	if _, err := my.DB.Exec(insertStmt, insertValues...); err != nil {
		return fmt.Errorf("error recording mapping: %s", err.Error())
	}

	return nil
}

func (my *MysqlStorage) QueryGitHubMapping(ssoIdentity string) (string, error) {
	row := my.DB.QueryRow("SELECT github_username FROM github_user_mappings WHERE sso_identity = ?", ssoIdentity)
	var githubUser string
	if err := row.Scan(&githubUser); err != nil {
		return "", err
	}

	return githubUser, nil
}

// Migrate runs any pending migrations
func (my *MysqlStorage) Migrate(migrationsDir string) error {
	gooseConf := goose.DBConf{
		MigrationsDir: migrationsDir,
		Env:           "sharkey",
		Driver: goose.DBDriver{
			Name:    "mysql",
			Import:  "github.com/go-sql-driver/mysql",
			Dialect: goose.MySqlDialect{},
		},
	}

	desiredVersion, err := goose.GetMostRecentDBVersion(migrationsDir)
	if err != nil {
		return fmt.Errorf("unable to run migrations: %s", err)
	}

	err = goose.RunMigrationsOnDb(&gooseConf, migrationsDir, desiredVersion, my.DB)
	if err != nil {
		return fmt.Errorf("unable to run migrations: %s", err)
	}

	return nil
}

func NewMysql(cfg config.Database) (*MysqlStorage, error) {
	url := cfg.Username
	if cfg.Password != "" {
		url += ":" + cfg.Password
	}
	url += "@tcp(" + cfg.Address + ")/" + cfg.Schema

	// Setup TLS (if configured)
	if cfg.TLS != nil {
		tlsConfig, err := config.BuildTLS(*cfg.TLS)
		if err != nil {
			return nil, err
		}
		err = mysql.RegisterTLSConfig("sharkey", tlsConfig)
		if err != nil {
			return nil, err
		}
		url += "?tls=sharkey"
	}

	db, err := sql.Open("mysql", url)
	return &MysqlStorage{DB: db}, err
}

// certTypeToMySQL converts the certType uint32 into a valid MySQL enum
// or returns error
func certTypeToMySQL(certType uint32) (string, error) {
	switch certType {
	case ssh.HostCert:
		return mysqlHostCert, nil
	case ssh.UserCert:
		return mysqlUserCert, nil
	default:
		return "", fmt.Errorf("storage: unknown ssh cert type: %d", certType)
	}
}
