package db

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
	"github.com/schollz/rwtxt/src/utils"
	"github.com/schollz/sqlite3dump"
	"github.com/schollz/versionedtext"
)

type FileSystem struct {
	name string
	db   *sql.DB
	sync.RWMutex
}

// File is the basic unit that is saved
type File struct {
	ID       string
	Slug     string
	Created  time.Time
	Modified time.Time
	Data     string
	Domain   string
	History  versionedtext.VersionedText
	DataHTML template.HTML
}

// New will initialize a filesystem
func New(name string) (fs *FileSystem, err error) {
	fs = new(FileSystem)
	if name == "" {
		err = errors.New("database must have name")
		return
	}
	fs.name = name

	// if read-only, make sure the database exists
	fs.db, err = sql.Open("sqlite3", ":memory:")
	if err != nil {
		return
	}
	err = fs.initializeDB()
	if err != nil {
		err = errors.Wrap(err, "could not initialize")
		return
	}
	return
}

func (fs *FileSystem) initializeDB() (err error) {
	if _, errHaveSQL := os.Stat(fs.name + ".sql.gz"); errHaveSQL == nil {
		fi, err := os.Open(fs.name + ".sql.gz")
		if err != nil {
			return err
		}
		defer fi.Close()

		fz, err := gzip.NewReader(fi)
		if err != nil {
			return err
		}
		defer fz.Close()

		s, err := ioutil.ReadAll(fz)
		if err != nil {
			return err
		}
		_, err = fs.db.Exec(string(s))
		return err
	}
	sqlStmt := `CREATE TABLE 
		fs (
			id TEXT NOT NULL PRIMARY KEY,
			domainid INTEGER,
			slug TEXT,
			created TIMESTAMP,
			modified TIMESTAMP,
			history TEXT
		);`
	_, err = fs.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "creating table")
		return
	}

	sqlStmt = `CREATE VIRTUAL TABLE 
		fts USING fts4 (id,data);`
	_, err = fs.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "creating virtual table")
	}

	sqlStmt = `CREATE TABLE 
	domains (
		id INTEGER NOT NULL PRIMARY KEY,
		name TEXT,
		key TEXT,
		public INTEGER
	);`
	_, err = fs.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "creating domains table")
	}

	sqlStmt = `CREATE TABLE 
	blobs (
		id TEXT NOT NULL PRIMARY KEY,
		name TEXT,
		data BLOB
	);`
	_, err = fs.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "creating domains table")
	}

	err = fs.setDomain("public", "")
	if err != nil {
		return
	}
	fs.DumpSQL()
	return
}

// DumpSQL will dump the SQL as text to filename.sql
func (fs *FileSystem) DumpSQL() (err error) {
	fs.Lock()
	defer fs.Unlock()

	// first purge the database of old stuff
	_, err = fs.db.Exec(`
	DELETE FROM fs WHERE id IN (SELECT id FROM fts where data == '');
	DELETE FROM fts WHERE data = '';
	`)
	if err != nil {
		return
	}

	fi, err := os.Create(fs.name + ".sql.gz")
	if err != nil {
		return
	}
	gf := gzip.NewWriter(fi)
	fw := bufio.NewWriter(gf)
	err = sqlite3dump.DumpDB(fs.db, fw)
	fw.Flush()
	gf.Close()
	fi.Close()
	return
}

// NewFile returns a new file
func (fs *FileSystem) NewFile(slug, data string) (f File) {
	f = File{
		ID:       utils.UUID(),
		Slug:     slug,
		Created:  time.Now(),
		Modified: time.Now(),
		Data:     data,
	}
	return
}

// SaveBlob will save a blob
func (fs *FileSystem) SaveBlob(id string, name string, blob []byte) (err error) {
	fs.Lock()
	defer fs.Unlock()

	tx, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin SaveBlob")
	}
	stmt, err := tx.Prepare(`
	INSERT OR REPLACE INTO
		blobs
	(
		id,
		name,
		data
	) 
		VALUES 	
	(
		?,
		?,
		?
	)`)
	if err != nil {
		return errors.Wrap(err, "stmt SaveBlob")
	}
	_, err = stmt.Exec(
		id, name, blob,
	)
	if err != nil {
		return errors.Wrap(err, "exec SaveBlob")
	}
	defer stmt.Close()
	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "commit SaveBlob")
	}
	return
}

// GetBlob will save a blob
func (fs *FileSystem) GetBlob(id string) (name string, data []byte, err error) {
	fs.Lock()
	defer fs.Unlock()

	stmt, err := fs.db.Prepare("SELECT name,data FROM blobs WHERE id = ?")
	if err != nil {
		return
	}
	defer stmt.Close()
	err = stmt.QueryRow(id).Scan(&name, &data)
	return
}

// Save a file to the file system. Will insert or ignore, and then update.
func (fs *FileSystem) Save(f File) (err error) {
	fs.Lock()
	defer fs.Unlock()

	// get current history and then update the history
	files, _ := fs.get(f.ID, f.Domain)
	if len(files) == 1 {
		f.History = files[0].History
		f.History.Update(f.Data)
	} else {
		f.History = versionedtext.NewVersionedText(f.Data)
	}
	// make sure domain exists
	if f.Domain == "" {
		f.Domain = "public"
	}
	domainid, _, _ := fs.getDomainFromName(f.Domain)
	if domainid == 0 {
		return errors.New("domain does not exist")
	}

	tx, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin Save")
	}

	stmt, err := tx.Prepare(`
	INSERT OR IGNORE INTO
		fs
	(
		id,
		domainid,
		slug,
		created,
		modified,
		history
	) 
		values 	
	(
		?, 
		?,
		?,
		?,
		?,
		?
	)`)
	if err != nil {
		return errors.Wrap(err, "stmt Save")
	}

	historyBytes, _ := json.Marshal(f.History)

	_, err = stmt.Exec(
		f.ID,
		domainid,
		f.Slug,
		f.Created,
		time.Now(),
		string(historyBytes),
	)
	if err != nil {
		return errors.Wrap(err, "exec Save")
	}
	defer stmt.Close()
	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "commit Save")
	}

	// if it was ignored
	tx2, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin Save")
	}
	stmt2, err := tx2.Prepare(`
	UPDATE fs SET 
		slug = ?,
		modified = ?,
		history = ?
	WHERE
		id = ?
	`)
	if err != nil {
		return errors.Wrap(err, "stmt update")
	}
	defer stmt2.Close()

	_, err = stmt2.Exec(
		f.Slug,
		time.Now(),
		string(historyBytes),
		f.ID,
	)
	if err != nil {
		return errors.Wrap(err, "exec update")
	}
	err = tx2.Commit()
	if err != nil {
		return errors.Wrap(err, "commit update")
	}

	// check if exists in fts
	sqlStmt := "INSERT INTO fts(data,id) VALUES (?,?)"
	var ftsHasID bool
	ftsHasID, err = fs.idExists(f.ID)
	if err != nil {
		return errors.Wrap(err, "doesExist")
	}
	if ftsHasID {
		sqlStmt = "UPDATE fts SET data=? WHERE id=?"
	}

	// update the index
	tx3, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin virtual Save")
	}
	stmt3, err := tx3.Prepare(sqlStmt)
	if err != nil {
		return errors.Wrap(err, "stmt virtual update")
	}
	defer stmt3.Close()

	_, err = stmt3.Exec(
		f.Data,
		f.ID,
	)
	if err != nil {
		return errors.Wrap(err, "exec virtual update")
	}
	err = tx3.Commit()
	if err != nil {
		return errors.Wrap(err, "commit virtual update")
	}
	return

}

// Close will make sure that the lock file is closed
func (fs *FileSystem) Close() (err error) {
	return fs.db.Close()
}

// Len returns how many things
func (fs *FileSystem) Len() (l int, err error) {
	fs.Lock()
	defer fs.Unlock()

	// prepare statement
	query := "SELECT COUNT(id) FROM FS"
	stmt, err := fs.db.Prepare(query)
	if err != nil {
		err = errors.Wrap(err, "preparing query: "+query)
		return
	}

	defer stmt.Close()
	rows, err := stmt.Query()
	if err != nil {
		err = errors.Wrap(err, query)
		return
	}

	// loop through rows
	defer rows.Close()
	for rows.Next() {
		err = rows.Scan(&l)
		if err != nil {
			err = errors.Wrap(err, "getRows")
			return
		}
	}
	err = rows.Err()
	if err != nil {
		err = errors.Wrap(err, "getRows")
	}
	return
}

// SetDomainPublicity will set the key of a domain, throws an error if it already exists
func (fs *FileSystem) SetDomainPublicity(domain string, publicity int) (err error) {
	// first check if it is a domain
	fs.Lock()
	defer fs.Unlock()

	tx, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin Save")
	}

	stmt, err := tx.Prepare(`
	UPDATE domains SET 
		public = ?
	WHERE
		name = ?
	`)
	if err != nil {
		return errors.Wrap(err, "stmt Save")
	}

	_, err = stmt.Exec(publicity, domain)
	if err != nil {
		return errors.Wrap(err, "exec Save")
	}
	defer stmt.Close()
	err = tx.Commit()
	return
}

// SetDomain will set the key of a domain, throws an error if it already exists
func (fs *FileSystem) SetDomain(domain, key string) (err error) {
	// first check if it is a domain
	fs.Lock()
	defer fs.Unlock()
	domainid, _, _ := fs.getDomainFromName(domain)
	if domainid != 0 {
		err = errors.New("domain already exists")
		return
	}
	return fs.setDomain(domain, key)
}

func (fs *FileSystem) setDomain(domain, key string) (err error) {
	domain = strings.ToLower(domain)

	tx, err := fs.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin Save")
	}

	stmt, err := tx.Prepare(`
	INSERT INTO
		domains
	(
		name, 
		key
	) 
		VALUES 	
	(
		?,
		?
	)`)
	if err != nil {
		return errors.Wrap(err, "stmt Save")
	}

	_, err = stmt.Exec(
		domain,
		key,
	)
	if err != nil {
		return errors.Wrap(err, "exec Save")
	}
	defer stmt.Close()
	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "commit Save")
	}

	log.Println("trying to insert", domain, key)

	return
}

// GetDomainID returns the domain id, throwing an error if it doesn't exist
func (fs *FileSystem) GetDomainFromName(domain string) (domainid int, key string, err error) {
	fs.Lock()
	defer fs.Unlock()
	domain = strings.ToLower(domain)
	domainid, key, err = fs.getDomainFromName(domain)
	if domainid == 0 {
		err = errors.New("domain " + domain + " does not exist")
	}
	return
}

func (fs *FileSystem) getDomainFromName(domain string) (domainid int, key string, err error) {
	// prepare statement
	query := "SELECT id,key FROM domains WHERE name = ?"
	stmt, err := fs.db.Prepare(query)
	if err != nil {
		err = errors.Wrap(err, "preparing query: "+query)
		return
	}

	defer stmt.Close()
	rows, err := stmt.Query(domain)
	if err != nil {
		err = errors.Wrap(err, query)
		return
	}

	// loop through rows
	defer rows.Close()
	for rows.Next() {
		err = rows.Scan(&domainid, &key)
		if err != nil {
			err = errors.Wrap(err, "getRows")
			return
		}
	}
	err = rows.Err()
	if err != nil {
		err = errors.Wrap(err, "getRows")
	}
	return
}

// GetTopX returns the info from a file
func (fs *FileSystem) GetTopX(domain string, num int) (files []File, err error) {
	fs.Lock()
	defer fs.Unlock()
	return fs.getAllFromPreparedQuery(`
	SELECT fs.id,fs.slug,fs.created,fs.modified,fts.data,fs.history FROM fs 
	INNER JOIN fts ON fs.id=fts.id 
	INNER JOIN domains ON fs.domainid=domains.id
	WHERE 
		domains.name = ?
	ORDER BY modified DESC LIMIT ?`, domain, num)
}

// Get returns the info from a file
func (fs *FileSystem) Get(id string, domain string) (files []File, err error) {
	fs.Lock()
	defer fs.Unlock()
	return fs.get(id, domain)
}

func (fs *FileSystem) get(id string, domain string) (files []File, err error) {

	files, err = fs.getAllFromPreparedQuery(`
		SELECT fs.id,fs.slug,fs.created,fs.modified,fts.data,fs.history FROM fs 
		INNER JOIN fts ON fs.id=fts.id 
		INNER JOIN domains ON fs.domainid=domains.id
		WHERE 
			fs.id = ? 
			AND
			domains.name = ?
		ORDER BY modified DESC`, id, domain)
	if err != nil {
		err = errors.Wrap(err, "get from id")
		return
	}
	if len(files) > 0 {
		return
	}

	files, err = fs.getAllFromPreparedQuery(`
	SELECT fs.id,fs.slug,fs.created,fs.modified,fts.data,fs.history 
	FROM fs 
	INNER JOIN fts ON fs.id=fts.id 
	INNER JOIN domains ON fs.domainid=domains.id
	WHERE 
		fs.id IN (SELECT id FROM fs WHERE slug=?) 
		AND
		domains.name = ?
		ORDER BY modified DESC`, id, domain)
	if err != nil {
		err = errors.Wrap(err, "get from slug")
		return
	}
	if len(files) > 0 {
		return
	}

	err = errors.New("no files with that slug or id")
	return
}

// LastModified get the last modified time
func (fs *FileSystem) LastModified() (lastModified time.Time, err error) {
	// prepare statement
	query := "SELECT modified FROM fs ORDER BY modified DESC LIMIT 1"
	stmt, err := fs.db.Prepare(query)
	if err != nil {
		err = errors.Wrap(err, "preparing query: "+query)
		return
	}

	defer stmt.Close()
	rows, err := stmt.Query()
	if err != nil {
		err = errors.Wrap(err, query)
		return
	}

	// loop through rows
	defer rows.Close()
	for rows.Next() {
		err = rows.Scan(&lastModified)
		if err != nil {
			err = errors.Wrap(err, "getRows")
			return
		}
	}
	err = rows.Err()
	if err != nil {
		err = errors.Wrap(err, "getRows")
	}
	return
}

// Find returns the info from a file
func (fs *FileSystem) Find(text string, domain string) (files []File, err error) {
	fs.Lock()
	defer fs.Unlock()

	files, err = fs.getAllFromPreparedQuery(`
		SELECT fs.id,fs.slug,fs.created,fs.modified,snippet(fts),fs.history FROM fts 
			INNER JOIN fs ON fs.id=fts.id 
			INNER JOIN domains ON fs.domainid=domains.id
			WHERE fts.data MATCH ?
			AND domains.name = ?
			ORDER BY modified DESC`, text, domain)
	return
}

// Exists returns whether specified ID exists exists
func (fs *FileSystem) idExists(id string) (exists bool, err error) {
	files, err := fs.getAllFromPreparedQuerySingleString(`
		SELECT id FROM fts WHERE id = ?`, id)
	if err != nil {
		err = errors.Wrap(err, "Exists")
	}
	if len(files) > 0 {
		exists = true
	}
	return
}

// Exists returns whether specified id or slug exists
func (fs *FileSystem) Exists(id string, domain string) (exists bool, err error) {
	fs.Lock()
	defer fs.Unlock()

	files, err := fs.getAllFromPreparedQuerySingleString(`
		SELECT fs.id FROM fs INNER JOIN domains ON fs.domainid=domains.id WHERE fs.id = ? AND domains.name = ?`, id, domain)
	if err != nil {
		err = errors.Wrap(err, "Exists")
		return
	}
	if len(files) > 0 {
		exists = true
		return
	}

	files, err = fs.getAllFromPreparedQuerySingleString(`
	SELECT fs.id FROM fs 
	INNER JOIN domains ON fs.domainid=domains.id
	WHERE fs.slug = ? AND domains.name = ?`, id, domain)
	if err != nil {
		err = errors.Wrap(err, "Exists")
		return
	}
	if len(files) > 0 {
		exists = true
	}

	return
}

func (fs *FileSystem) getAllFromPreparedQuery(query string, args ...interface{}) (files []File, err error) {
	// prepare statement
	stmt, err := fs.db.Prepare(query)
	if err != nil {
		err = errors.Wrap(err, "preparing query: "+query)
		return
	}

	defer stmt.Close()
	rows, err := stmt.Query(args...)
	if err != nil {
		err = errors.Wrap(err, query)
		return
	}

	// loop through rows
	defer rows.Close()
	files = []File{}
	for rows.Next() {
		var f File
		var history sql.NullString
		err = rows.Scan(
			&f.ID,
			&f.Slug,
			&f.Created,
			&f.Modified,
			&f.Data,
			&history,
		)
		if err != nil {
			err = errors.Wrap(err, "get rows of file")
			return
		}
		if history.Valid {
			err = json.Unmarshal([]byte(history.String), &f.History)
			if err != nil {
				err = errors.Wrap(err, "could not parse history")
				return
			}
		}
		f.DataHTML = template.HTML(f.Data)
		files = append(files, f)
	}
	err = rows.Err()
	if err != nil {
		err = errors.Wrap(err, "getRows")
	}
	return
}

func (fs *FileSystem) getAllFromPreparedQuerySingleString(query string, args ...interface{}) (s []string, err error) {
	// prepare statement
	stmt, err := fs.db.Prepare(query)
	if err != nil {
		err = errors.Wrap(err, "preparing query: "+query)
		return
	}

	defer stmt.Close()
	rows, err := stmt.Query(args...)
	if err != nil {
		err = errors.Wrap(err, query)
		return
	}

	// loop through rows
	defer rows.Close()
	s = []string{}
	for rows.Next() {
		var stemp string
		err = rows.Scan(
			&stemp,
		)
		if err != nil {
			err = errors.Wrap(err, "getRows")
			return
		}
		s = append(s, stemp)
	}
	err = rows.Err()
	if err != nil {
		err = errors.Wrap(err, "getRows")
	}
	return
}
