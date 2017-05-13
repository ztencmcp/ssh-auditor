package sshauditor

import (
	"database/sql"
	"fmt"
	"log"
	"net"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
	hostport character varying,
	version character varying,
	fingerprint character varying,
	seen_first REAL,
	seen_last REAL,

	PRIMARY KEY (hostport)
);

CREATE TABLE IF NOT EXISTS credentials (
	user character varying,
	password character varying,
	priority DEFAULT 0,

	PRIMARY KEY (user, password)
);

CREATE TABLE IF NOT EXISTS host_creds (
	hostport character varying,
	user character varying,
	password character varying,
	last_tested REAL,
	result character varying,
	priority DEFAULT 0,

	PRIMARY KEY (hostport, user, password)
);

CREATE TABLE IF NOT EXISTS host_changes (
	time REAL,
	hostport character varying,
	type character varying,
	old character varying,
	new character varying
)
`

type Host struct {
	Hostport    string
	Version     string
	Fingerprint string
	SeenFirst   string `db:"seen_first"`
	SeenLast    string `db:"seen_last"`
}

type Credential struct {
	User     string
	Password string
	Priority int
}

func (c Credential) String() string {
	return fmt.Sprintf("%s:%s", c.User, c.Password)
}

type HostCredential struct {
	Hostport   string
	User       string
	Password   string
	LastTested string `db:"last_tested"`
	Result     string
	Priority   int
}

type SQLiteStore struct {
	conn    *sqlx.DB
	tx      *sqlx.Tx
	txDepth int
}

func NewSQLiteStore(uri string) (*SQLiteStore, error) {
	conn, err := sqlx.Open("sqlite3", uri)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{conn: conn}, nil
}

func (s *SQLiteStore) Close() error {
	return s.conn.Close()
}

func (s *SQLiteStore) Init() error {
	_, err := s.conn.Exec(schema)
	return errors.Wrap(err, "Init() failed")
}

func (s *SQLiteStore) Begin() (*sqlx.Tx, error) {
	if s.tx != nil {
		s.txDepth += 1
		//log.Printf("Returning existing transaction: depth=%d\n", s.txDepth)
		return s.tx, nil
	}
	//log.Printf("new transaction\n")
	tx, err := s.conn.Beginx()
	if err != nil {
		return tx, err
	}
	s.tx = tx
	s.txDepth += 1
	return s.tx, nil
}

func (s *SQLiteStore) Commit() error {
	if s.tx == nil {
		return errors.New("Commit outside of transaction")
	}
	s.txDepth -= 1
	if s.txDepth > 0 {
		//log.Printf("Not commiting stacked transaction: depth=%d\n", s.txDepth)
		return nil // No OP
	}
	//log.Printf("Commiting transaction: depth=%d\n", s.txDepth)
	err := s.tx.Commit()
	s.tx = nil
	return err
}

func (s *SQLiteStore) Exec(query string, args ...interface{}) (sql.Result, error) {
	tx, err := s.Begin()
	defer s.Commit()
	if err != nil {
		return nil, err
	}
	return tx.Exec(query, args...)
}
func (s *SQLiteStore) Select(dest interface{}, query string, args ...interface{}) error {
	tx, err := s.Begin()
	defer s.Commit()
	if err != nil {
		return err
	}
	return tx.Select(dest, query, args...)
}

func (s *SQLiteStore) AddCredential(c Credential) error {
	_, err := s.Exec(
		"INSERT INTO credentials (user, password, priority) VALUES ($1, $2, $3)",
		c.User, c.Password, c.Priority)
	return err
}

func (s *SQLiteStore) getKnownHosts() (map[string]Host, error) {
	hostList := []Host{}

	hosts := make(map[string]Host)

	err := s.Select(&hostList, "SELECT * FROM hosts")
	if err != nil {
		return hosts, err
	}
	for _, h := range hostList {
		hosts[h.Hostport] = h
	}
	return hosts, nil
}

func (s *SQLiteStore) resetHostCreds(h SSHHost) error {
	_, err := s.Exec("UPDATE host_creds set last_tested=0 where hostport=$1", h.hostport)
	return err
}

func (s *SQLiteStore) addOrUpdateHost(h SSHHost) error {
	err := s.resetHostCreds(h)
	if err != nil {
		return err
	}
	res, err := s.Exec(
		`UPDATE hosts SET version=$1,fingerprint=$2,seen_last=datetime('now', 'localtime')
			WHERE hostport=$3`,
		h.version, h.keyfp, h.hostport)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if rows != 0 {
		return err
	}
	_, err = s.Exec(
		`INSERT INTO hosts (hostport, version, fingerprint, seen_first, seen_last) VALUES
			($1, $2, $3, datetime('now', 'localtime'), datetime('now', 'localtime'))`,
		h.hostport, h.version, h.keyfp)
	return err
}

func (s *SQLiteStore) setLastSeen(h SSHHost) error {
	_, err := s.Exec(
		"UPDATE hosts SET seen_last=datetime('now', 'localtime') WHERE hostport=$1",
		h.hostport)
	return err
}

func (s *SQLiteStore) addHostChange(h SSHHost, changeType, old, new string) error {
	q := `INSERT INTO host_changes (time, hostport, type, old, new) VALUES
			(datetime('now', 'localtime'), $1, $2, $3, $4)`
	_, err := s.Exec(q, h.hostport, changeType, old, new)
	return errors.Wrap(err, "addHostChanges failed")
}

func (s *SQLiteStore) addHostChanges(new SSHHost, old Host) error {
	var err error
	if old.Fingerprint != new.keyfp {
		err = s.addHostChange(new, "fingerprint", old.Fingerprint, new.keyfp)
		if err != nil {
			return err
		}
	}
	if old.Version != new.version {
		err = s.addHostChange(new, "version", old.Version, new.version)
	}
	return err
}

func (s *SQLiteStore) getAllCreds() ([]Credential, error) {
	credentials := []Credential{}
	err := s.Select(&credentials, "SELECT * from credentials")
	return credentials, err
}

func (s *SQLiteStore) initHostCreds() (int, error) {
	_, err := s.Begin()
	defer s.Commit()
	if err != nil {
		return 0, err
	}
	creds, err := s.getAllCreds()
	if err != nil {
		return 0, err
	}

	knownHosts, err := s.getKnownHosts()
	if err != nil {
		return 0, err
	}

	inserted := 0
	for _, host := range knownHosts {
		ins, err := s.initHostCredsForHost(creds, host)
		if err != nil {
			return inserted, err
		}
		inserted += ins
	}
	return inserted, nil
}
func (s *SQLiteStore) initHostCredsForHost(creds []Credential, h Host) (int, error) {
	inserted := 0
	for _, c := range creds {
		res, err := s.Exec(`INSERT OR IGNORE INTO host_creds (hostport, user, password, last_tested, result, priority) VALUES
			($1, $2, $3, 0, '', $4)`,
			h.Hostport, c.User, c.Password, c.Priority)
		if err != nil {
			return inserted, err
		}
		rows, err := res.RowsAffected()
		inserted += int(rows)
	}
	return inserted, nil
}

func (s *SQLiteStore) getScanQueueHelper(query string) ([]ScanRequest, error) {
	requestMap := make(map[string]*ScanRequest)
	var requests []ScanRequest
	credentials := []HostCredential{}
	err := s.Select(&credentials, query)
	if err != nil {
		return requests, err
	}

	for _, hc := range credentials {
		sr := requestMap[hc.Hostport]
		if sr == nil {
			sr = &ScanRequest{
				host: Host{Hostport: hc.Hostport},
			}
		}
		sr.credentials = append(sr.credentials, Credential{User: hc.User, Password: hc.Password})
		requestMap[hc.Hostport] = sr
	}

	for _, sr := range requestMap {
		requests = append(requests, *sr)
	}

	return requests, nil
}
func (s *SQLiteStore) getScanQueue() ([]ScanRequest, error) {
	q := `select host_creds.* from host_creds, hosts
		where hosts.hostport = host_creds.hostport and
		last_tested < datetime('now', '-14 day') and
		hosts.fingerprint != '' and
		seen_last > datetime('now', '-30 day') order by last_tested ASC limit 5000`
	return s.getScanQueueHelper(q)
}
func (s *SQLiteStore) getRescanQueue() ([]ScanRequest, error) {
	q := `select * from host_creds where result !='' order by last_tested ASC limit 5000`
	return s.getScanQueueHelper(q)
}

func (s *SQLiteStore) updateBruteResult(br BruteForceResult) error {
	_, err := s.Exec(`UPDATE host_creds set last_tested=datetime('now', 'localtime'), result=$1
		WHERE hostport=$2 AND user=$3 AND password=$4`,
		br.result, br.host.Hostport, br.cred.User, br.cred.Password)
	return err
}

func (s *SQLiteStore) duplicateKeyReport() error {
	hosts := []Host{}

	err := s.Select(&hosts, "SELECT * FROM hosts where fingerprint != ''")

	if err != nil {
		return err
	}

	keyMap := make(map[string][]Host)

	for _, h := range hosts {
		keyMap[h.Fingerprint] = append(keyMap[h.Fingerprint], h)
	}

	for fp, hosts := range keyMap {
		if len(hosts) == 1 {
			continue
		}
		fmt.Printf("Key %s in use by %d hosts:\n", fp, len(hosts))
		for _, h := range hosts {
			fmt.Printf(" %s\n", h.Hostport)
		}
		fmt.Println()
	}
	return nil
}

func (s *SQLiteStore) getLogCheckQueue() ([]ScanRequest, error) {
	requestMap := make(map[string]*ScanRequest)
	var requests []ScanRequest
	hostList := []Host{}
	query := `SELECT * FROM hosts WHERE seen_last > datetime('now', '-30 day')`
	err := s.Select(&hostList, query)
	if err != nil {
		return requests, err
	}

	for _, h := range hostList {
		host, _, err := net.SplitHostPort(h.Hostport)
		if err != nil {
			log.Printf("Bad hostport? %s %s", h.Hostport, err)
			continue
		}
		user := fmt.Sprintf("logcheck-%s", host)

		sr := requestMap[h.Hostport]
		if sr == nil {
			sr = &ScanRequest{
				host: Host{Hostport: h.Hostport},
			}
		}
		sr.credentials = append(sr.credentials, Credential{User: user, Password: "logcheck"})
		requestMap[h.Hostport] = sr
	}

	for _, sr := range requestMap {
		requests = append(requests, *sr)
	}

	return requests, nil
}