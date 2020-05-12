// +build postgres

// Package postgres is a database adapter for Postgres
package postgres

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/tinode/chat/server/auth"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"
)

// adapter holds Postgres connection data.
type adapter struct {
	db     *sqlx.DB
	dsn    string
	dbName string
	// Maximum number of records to return
	maxResults int
	version    int
}

const (
	defaultDSN      = "postgres://postgres@localhost:5432"
	defaultDatabase = "tinode"

	adpVersion = 108

	adapterName = "pg"

	defaultMaxResults = 1024
)

type configType struct {
	DSN    string `json:"dsn,omitempty"`
	DBName string `json:"database,omitempty"`
}

// Open initializes database session
func (a *adapter) Open(jsonconfig string) error {
	if a.db != nil {
		return errors.New("postgres adapter is already connected")
	}

	var err error
	var config configType

	if err = json.Unmarshal([]byte(jsonconfig), &config); err != nil {
		return errors.New("postgres adapter failed to parse config: " + err.Error())
	}

	a.dsn = config.DSN
	if a.dsn == "" {
		a.dsn = defaultDSN
	}

	a.dbName = config.DBName
	if a.dbName == "" {
		a.dbName = defaultDatabase
	}

	if a.maxResults <= 0 {
		a.maxResults = defaultMaxResults
	}

	fmt.Println("Opening the Db connection")
	// This just initializes the driver but does not open the network connection.
	a.db, err = sqlx.Open("postgres", a.dsn)
	if err != nil {
		return err
	}

	// Actually opening the network connection.
	fmt.Println("Pinging the Db connection")
	err = a.db.Ping()
	if isMissingDb(err) {
		// Ignore missing database here. If we are initializing the database
		// missing DB is OK.
		err = nil
	}
	return err
}

// Close closes the underlying database connection
func (a *adapter) Close() error {
	var err error
	if a.db != nil {
		err = a.db.Close()
		a.db = nil
		a.version = -1
	}
	return err
}

// IsOpen returns true if connection to database has been established. It does not check if
// connection is actually live.
func (a *adapter) IsOpen() bool {
	fmt.Println("inside adapter isOpen()")
	return a.db != nil
}

// GetDbVersion returns current database version.
func (a *adapter) GetDbVersion() (int, error) {
	if a.version > 0 {
		return a.version, nil
	}

	var vers int
	err := a.db.Get(&vers, "SELECT value FROM kvmeta WHERE key='version'")
	if err != nil {
		if isMissingDb(err) || err == sql.ErrNoRows {
			err = errors.New("Database not initialized")
		}
		return -1, err
	}

	a.version = vers

	return vers, nil
}

func (a *adapter) updateDbVersion(v int) error {
	a.version = -1
	if _, err := a.db.Exec("UPDATE kvmeta SET `value`=$1 WHERE `key`='version'", v); err != nil {
		return err
	}
	return nil
}

// CheckDbVersion checks whether the actual DB version matches the expected version of this adapter.
func (a *adapter) CheckDbVersion() error {
		version, err := a.GetDbVersion()
		if err != nil {
			return err
		}

		if version != adpVersion {
			return errors.New("Invalid database version " + strconv.Itoa(version) +
				". Expected " + strconv.Itoa(adpVersion))
		}
	return nil
}

// Version returns adapter version.
func (adapter) Version() int {
	return adpVersion
}

// GetName returns string that adapter uses to register itself with store.
func (a *adapter) GetName() string {
	return adapterName
}

// SetMaxResults configures how many results can be returned in a single DB call.
func (a *adapter) SetMaxResults(val int) error {
	if val <= 0 {
		a.maxResults = defaultMaxResults
	} else {
		a.maxResults = val
	}

	return nil
}

// CreateDb initializes the storage.
func (a *adapter) CreateDb(reset bool) error {
	var err error
	var tx *sql.Tx

	// Can't use an existing connection because it's configured with a database name which may not exist.
	// Don't care if it does not close cleanly.
	a.db.Close()

	// Clear database name
	startIndex := strings.LastIndex(a.dsn, "/") + 1
	endIndex := strings.Index(a.dsn, "?")
	newDsn := strings.Replace(a.dsn, "/"+a.dsn[startIndex:endIndex], "", 1)

	a.db, err = sqlx.Open("postgres", newDsn)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	fmt.Println("droping db")
	result := a.db.MustExec("DROP DATABASE IF EXISTS " + a.dbName)
	fmt.Println("dropped db")
	fmt.Println(result)

	result = a.db.MustExec("CREATE DATABASE " + a.dbName)
	fmt.Println("created db")
	fmt.Println(result)
	a.db.Close()

	a.db, err = sqlx.Connect("postgres", a.dsn)
	if err != nil {
		return err
	}

	if tx, err = a.db.Begin(); err != nil {
		return err
	}

	fmt.Println("connected")

	if _, err = tx.Exec(
		`CREATE TABLE users(
			id        BIGINT NOT NULL,
			createdat TIMESTAMP NOT NULL,
			updatedat TIMESTAMP NOT NULL,
			deletedat TIMESTAMP,
			state     INT DEFAULT 0,
			access    JSON,
			lastseen  TIMESTAMP,
			useragent VARCHAR(255) DEFAULT '',
			public    JSON,
			tags      JSON,
			PRIMARY KEY(id)
		)`); err != nil {
		return err
	}

	fmt.Println("users created")
	if _, err = tx.Exec(`CREATE INDEX users_deletedat ON users(deletedat)`); err != nil {
		return err
	}
	fmt.Println("user tags created")

	// Indexed user tags.
	if _, err = tx.Exec(
		`CREATE TABLE usertags(
			id     SERIAL,
			userid BIGINT NOT NULL,
			tag    VARCHAR(96) NOT NULL,
			PRIMARY KEY(id),
			FOREIGN KEY(userid) REFERENCES users(id),
			CONSTRAINT usertags_userid_tag UNIQUE(userid, tag)
		)`); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE INDEX usertags_tag ON usertags(tag)`); err != nil {
		return err
	}

	fmt.Println("devices created:")
	// Indexed devices. Normalized into a separate table.
	if _, err = tx.Exec(
		`CREATE TABLE devices(
			id       SERIAL,
			userid   BIGINT NOT NULL,
			hash     CHAR(16) NOT NULL,
			deviceid TEXT NOT NULL,
			platform VARCHAR(32),
			lastseen TIMESTAMP NOT NULL,
			lang     VARCHAR(8),
			PRIMARY KEY(id),
			FOREIGN KEY(userid) REFERENCES users(id),
			CONSTRAINT devices_hash UNIQUE(hash)
		)`); err != nil {
		return err
	}

	// Authentication records for the basic authentication scheme.
	if _, err = tx.Exec(
		`CREATE TABLE auth(
			id      SERIAL,
			uname   VARCHAR(32) NOT NULL,
			userid  BIGINT NOT NULL,
			scheme  VARCHAR(16) NOT NULL,
			authlvl INT NOT NULL,
			secret  VARCHAR(255) NOT NULL,
			expires TIMESTAMP,
			PRIMARY KEY(id),
			FOREIGN KEY(userid) REFERENCES users(id),
			CONSTRAINT auth_userid_scheme UNIQUE(userid, scheme),
			CONSTRAINT auth_uname UNIQUE(uname)
		)`); err != nil {
		return err
	}

	fmt.Println("auth created:")
	// Topics
	if _, err = tx.Exec(
		`CREATE TABLE topics(
			id        SERIAL,
			createdat TIMESTAMP NOT NULL,
			updatedat TIMESTAMP NOT NULL,
			deletedat TIMESTAMP,
			touchedat TIMESTAMP,
			name      CHAR(25) NOT NULL,
			usebt     INT DEFAULT 0,
			owner     BIGINT NOT NULL DEFAULT 0,
			access    JSON,
			seqid     INT NOT NULL DEFAULT 0,
			delid     INT DEFAULT 0,
			public    JSON,
			tags      JSON,
			PRIMARY KEY(id),
			CONSTRAINT topics_name UNIQUE(name)
		)`); err != nil {
		return err
	}

	fmt.Println("topics created:")
	if _, err = tx.Exec(`CREATE INDEX topics_owners ON topics(owner)`); err != nil {
		return err
	}

	fmt.Println("topicstags creating:")
	// Indexed topic tags.
	if _, err = tx.Exec(
		`CREATE TABLE topictags(
			id    SERIAL,
			topic CHAR(25) NOT NULL,
			tag   VARCHAR(96) NOT NULL,
			PRIMARY KEY(id),
			FOREIGN KEY(topic) REFERENCES topics(name),
			CONSTRAINT topictags_userid_tag UNIQUE(topic, tag)
		)`); err != nil {
		return err
	}

	if _, err = tx.Exec(`CREATE INDEX topictags_tag ON topictags(tag)`); err != nil {
		return err
	}

	fmt.Println("subscription creating")
	// Subscriptions
	if _, err = tx.Exec(
		`CREATE TABLE subscriptions(
			id         SERIAL,
			createdat  TIMESTAMP NOT NULL,
			updatedat  TIMESTAMP NOT NULL,
			deletedat  TIMESTAMP,
			userid     BIGINT NOT NULL,
			topic      CHAR(25) NOT NULL,
			delid      INT DEFAULT 0,
			recvseqid  INT DEFAULT 0,
			readseqid  INT DEFAULT 0,
			modewant   CHAR(8),
			modegiven  CHAR(8),
			private    JSON,
			PRIMARY KEY(id),
			FOREIGN KEY(userid) REFERENCES users(id),
			CONSTRAINT subscriptions_topic_userid UNIQUE(topic, userid)
		)`); err != nil {
		return err
	}

	fmt.Println("crating subscription index")
	if _, err = tx.Exec(`CREATE INDEX subscriptions_topic ON subscriptions(topic)`); err != nil {
		return err
	}

	fmt.Println("creating messages")
	// Messages
	if _, err = tx.Exec(
		`CREATE TABLE messages(
			id        SERIAL,
			createdat TIMESTAMP NOT NULL,
			updatedat TIMESTAMP NOT NULL,
			deletedat TIMESTAMP,
			delid     INT DEFAULT 0,
			seqid     INT NOT NULL,
			topic     CHAR(25) NOT NULL,
			"from"  BIGINT NOT NULL,
			head     	JSON,
			content   JSON,
			PRIMARY KEY(id),
			FOREIGN KEY(topic) REFERENCES topics(name),
			CONSTRAINT messages_topic_seqid UNIQUE(topic, seqid)
		)`); err != nil {
		return err
	}

	fmt.Println("creating dellog")
	// Deletion log
	if _, err = tx.Exec(
		`CREATE TABLE dellog(
			id         SERIAL,
			topic      VARCHAR(25) NOT NULL,
			deletedfor BIGINT NOT NULL DEFAULT 0,
			delid      INT NOT NULL,
			low        INT NOT NULL,
			hi         INT NOT NULL,
			PRIMARY KEY(id),
			FOREIGN KEY(topic) REFERENCES topics(name)
		);`); err != nil {
		return err
	}

	if _, err = tx.Exec(`CREATE INDEX dellog_topic_delid_deletedfor ON dellog(topic, delid, deletedfor)`); err != nil {
		return err
	}

	if _, err = tx.Exec(`CREATE INDEX dellog_topic_deletedfor_low_hi ON dellog(topic, deletedfor, low, hi)`); err != nil {
		return err
	}

	if _, err = tx.Exec(`CREATE INDEX dellog_deletedfor ON dellog(deletedfor)`); err != nil {
		return err
	}

	// User credentials
	if _, err = tx.Exec(
		`CREATE TABLE credentials(
			id        SERIAL,
			createdat TIMESTAMP NOT NULL,
			updatedat TIMESTAMP NOT NULL,
			deletedat TIMESTAMP,
			method    VARCHAR(16) NOT NULL,
			value     VARCHAR(128) NOT NULL,
			synthetic VARCHAR(192) NOT NULL,
			userid    BIGINT NOT NULL,
			resp      VARCHAR(255),
			done      boolean NOT NULL DEFAULT false,
			retries   INT NOT NULL DEFAULT 0,
			PRIMARY KEY(id),
			CONSTRAINT credentials_uniqueness UNIQUE(synthetic),
			FOREIGN KEY(userid) REFERENCES users(id)
		);`); err != nil {
		return err
	}

	// Records of uploaded files.
	// Don't add FOREIGN KEY on userid. It's not needed and it will break user deletion.
	if _, err = tx.Exec(
		`CREATE TABLE fileuploads(
			id        BIGINT NOT NULL,
			createdat TIMESTAMP NOT NULL,
			updatedat TIMESTAMP NOT NULL,
			userid    BIGINT NOT NULL,
			status    INT NOT NULL,
			mimetype  VARCHAR(255) NOT NULL,
			size      BIGINT NOT NULL,
			location  VARCHAR(2048) NOT NULL,
			PRIMARY KEY(id)
		)`); err != nil {
		return err
	}

	// Links between uploaded files and the messages they are attached to.
	if _, err = tx.Exec(
		`CREATE TABLE filemsglinks(
			id			SERIAL,
			createdat	TIMESTAMP NOT NULL,
			fileid		BIGINT NOT NULL,
			msgid 		INT NOT NULL,
			PRIMARY KEY(id),
			FOREIGN KEY(fileid) REFERENCES fileuploads(id) ON DELETE CASCADE,
			FOREIGN KEY(msgid) REFERENCES messages(id) ON DELETE CASCADE
		)`); err != nil {
		return err
	}

	fmt.Println("creating kvmeta")
	if _, err = tx.Exec(
		`CREATE TABLE kvmeta(
			key CHAR(32),
			value TEXT,
			PRIMARY KEY(KEY)
		)`); err != nil {
		return err
	}
	fmt.Println("inserting kvmeta")
	if _, err = tx.Exec("INSERT INTO kvmeta(key, value) VALUES ('version'," + strconv.Itoa(adpVersion) + ")"); err != nil {
		return err
	}

	fmt.Println("creation db complete")
	return tx.Commit()
}

func (a *adapter) UpgradeDb() error {
	//  Postgress adapter does not exist for older versions and consequently does not need to support the upgrades.
	return nil
}

func addTags(tx *sqlx.Tx, table, keyName string, keyVal interface{}, tags []string, ignoreDups bool) error {
	if len(tags) == 0 {
		return nil
	}

	var insert *sql.Stmt
	var err error

	insert, err = tx.Prepare("INSERT INTO " + table + "(" + keyName + ",tag) VALUES($1, $2)")
	if err != nil {
		return err
	}

	for _, tag := range tags {
		_, err = insert.Exec(keyVal, tag)

		if err != nil {
			if isDupe(err) {
				if ignoreDups {
					continue
				}
				return t.ErrDuplicate
			}
			return err
		}
	}

	return nil
}

func removeTags(tx *sqlx.Tx, table, keyName string, keyVal interface{}, tags []string) error {
	if len(tags) == 0 {
		return nil
	}

	var args []interface{}
	for _, tag := range tags {
		args = append(args, tag)
	}

	query, args, _ := sqlx.In("DELETE FROM "+table+" WHERE "+keyName+"=$1 AND tag IN ($2)", keyVal, args)
	query = tx.Rebind(query)
	_, err := tx.Exec(query, args...)

	return err
}

// UserCreate creates a new user. Returns error and true if error is due to duplicate user name,
// false for any other error
func (a *adapter) UserCreate(user *t.User) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	decoded_uid := store.DecodeUid(user.Uid())

	if _, err = tx.Exec("INSERT INTO users(id,createdat,updatedat,access,public,tags) VALUES($1,$2,$3,$4,$5,$6)",
		decoded_uid,
		user.CreatedAt, user.UpdatedAt,
		toJSON(user.Access), toJSON(user.Public), toJSON(user.Tags)); err != nil {
		return err
	}

	// Save user's tags to a separate table to make user findable.
	if err = addTags(tx, "usertags", "userid", decoded_uid, user.Tags, false); err != nil {
		return err
	}

	return tx.Commit()
}

// Add user's authentication record
func (a *adapter) AuthAddRecord(uid t.Uid, scheme, unique string, authLvl auth.Level,
	secret []byte, expires time.Time) (bool, error) {

	var exp *time.Time
	if !expires.IsZero() {
		exp = &expires
	}
	_, err := a.db.Exec("INSERT INTO auth(uname,userid,scheme,authLvl,secret,expires) VALUES($1,$2,$3,$4,$5,$6)",
		unique, store.DecodeUid(uid), scheme, authLvl, secret, exp)
	if err != nil {
		if isDupe(err) {
			return true, t.ErrDuplicate
		}
		return false, err
	}
	return false, nil
}

// AuthDelScheme deletes an existing authentication scheme for the user.
func (a *adapter) AuthDelScheme(user t.Uid, scheme string) error {
	_, err := a.db.Exec("DELETE FROM auth WHERE userid=$1 AND scheme=$2", store.DecodeUid(user), scheme)
	return err
}

// AuthDelAllRecords deletes all authentication records for the user.
func (a *adapter) AuthDelAllRecords(user t.Uid) (int, error) {
	res, err := a.db.Exec("DELETE FROM auth WHERE userid=$1", store.DecodeUid(user))
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()

	return int(count), nil
}

// Update user's authentication secret
func (a *adapter) AuthUpdRecord(uid t.Uid, scheme, unique string, authLvl auth.Level,
	secret []byte, expires time.Time) (bool, error) {
	var exp *time.Time
	if !expires.IsZero() {
		exp = &expires
	}

	_, err := a.db.Exec("UPDATE auth SET uname=$1,authLvl=$2,secret=$3,expires=$4 WHERE uname=$5",
		unique, authLvl, secret, exp, unique)
	if isDupe(err) {
		return true, t.ErrDuplicate
	}

	return false, err
}

// Retrieve user's authentication record
func (a *adapter) AuthGetRecord(uid t.Uid, scheme string) (string, auth.Level, []byte, time.Time, error) {
	var expires time.Time

	var record struct {
		Uname   string
		Authlvl auth.Level
		Secret  []byte
		Expires *time.Time
	}

	if err := a.db.Get(&record, "SELECT uname,secret,expires,authlvl FROM auth WHERE userid=$1 AND scheme=$2",
		store.DecodeUid(uid), scheme); err != nil {
		if err == sql.ErrNoRows {
			// Nothing found - clear the error
			err = nil
		}
		return "", 0, nil, expires, err
	}

	if record.Expires != nil {
		expires = *record.Expires
	}

	return record.Uname, record.Authlvl, record.Secret, expires, nil
}

// Retrieve user's authentication record
func (a *adapter) AuthGetUniqueRecord(unique string) (t.Uid, auth.Level, []byte, time.Time, error) {
	var expires time.Time

	var record struct {
		Userid  int64
		Authlvl auth.Level
		Secret  []byte
		Expires *time.Time
	}

	if err := a.db.Get(&record, "SELECT userid,secret,expires,authlvl FROM auth WHERE uname=$1", unique); err != nil {
		if err == sql.ErrNoRows {
			// Nothing found - clear the error
			err = nil
		}
		return t.ZeroUid, 0, nil, expires, err
	}

	if record.Expires != nil {
		expires = *record.Expires
	}

	return store.EncodeUid(record.Userid), record.Authlvl, record.Secret, expires, nil
}

// UserGet fetches a single user by user id. If user is not found it returns (nil, nil)
func (a *adapter) UserGet(uid t.Uid) (*t.User, error) {
	var user t.User
	err := a.db.Get(&user, "SELECT * FROM users WHERE id=$1 AND deletedat IS NULL", store.DecodeUid(uid))
	if err == nil {
		user.SetUid(uid)
		user.Public = fromJSON(user.Public)
		return &user, nil
	}

	if err == sql.ErrNoRows {
		// Clear the error if user does not exist or marked as soft-deleted.
		return nil, nil
	}

	// If user does not exist, it returns nil, nil
	return nil, err
}

func (a *adapter) UserGetAll(ids ...t.Uid) ([]t.User, error) {
	uids := make([]interface{}, len(ids))
	for i, id := range ids {
		uids[i] = store.DecodeUid(id)
	}

	users := []t.User{}
	q, uids, _ := sqlx.In("SELECT * FROM users WHERE id IN ($1) AND deletedat IS NULL", uids)
	q = a.db.Rebind(q)
	rows, err := a.db.Queryx(q, uids...)
	if err != nil {
		return nil, err
	}

	var user t.User
	for rows.Next() {
		if err = rows.StructScan(&user); err != nil {
			users = nil
			break
		}

		if user.DeletedAt != nil {
			continue
		}

		user.SetUid(encodeUidString(user.Id))
		user.Public = fromJSON(user.Public)

		users = append(users, user)
	}
	rows.Close()

	return users, err
}

// UserDelete deletes specified user: wipes completely (hard-delete) or marks as deleted.
// TODO: report when the user is not found.
func (a *adapter) UserDelete(uid t.Uid, hard bool) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	decoded_uid := store.DecodeUid(uid)

	if hard {
		// Delete user's devices
		if err = deviceDelete(tx, uid, ""); err != nil {
			return err
		}

		// Delete user's subscriptions in all topics.
		if err = subsDelForUser(tx, uid, true); err != nil {
			return err
		}

		// Delete records of messages soft-deleted for the user.
		if _, err = tx.Exec("DELETE FROM dellog WHERE deletedfor=$1", decoded_uid); err != nil {
			return err
		}

		// Can't delete user's messages in all topics because we cannot notify topics of such deletion.
		// Just leave the messages there marked as sent by "not found" user.

		// Delete topics where the user is the owner.

		// First delete all messages in those topics.
		if _, err = tx.Exec("DELETE dellog FROM dellog LEFT JOIN topics ON topics.name=dellog.topic WHERE topics.owner=$1",
			decoded_uid); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE messages FROM messages LEFT JOIN topics ON topics.name=messages.topic WHERE topics.owner=$2",
			decoded_uid); err != nil {
			return err
		}

		// Delete all subscriptions.
		if _, err = tx.Exec("DELETE sub FROM subscriptions AS sub LEFT JOIN topics ON topics.name=sub.topic WHERE topics.owner=$1",
			decoded_uid); err != nil {
			return err
		}

		// Delete topic tags
		if _, err = tx.Exec("DELETE topictags FROM topictags LEFT JOIN topics ON topics.name=topictags.topic WHERE topics.owner=$1",
			decoded_uid); err != nil {
			return err
		}

		// And finally delete the topics.
		if _, err = tx.Exec("DELETE FROM topics WHERE owner=$1", decoded_uid); err != nil {
			return err
		}

		// Delete user's authentication records.
		if _, err = tx.Exec("DELETE FROM auth WHERE userid=$1", decoded_uid); err != nil {
			return err
		}

		// Delete all credentials.
		if err = credDel(tx, uid, "", ""); err != nil {
			return err
		}

		if _, err = tx.Exec("DELETE FROM usertags WHERE userid=$1", decoded_uid); err != nil {
			return err
		}

		if _, err = tx.Exec("DELETE FROM users WHERE id=$1", decoded_uid); err != nil {
			return err
		}
	} else {
		now := t.TimeNow()
		// Disable all user's subscriptions. That includes p2p subscriptions. No need to delete them.
		if err = subsDelForUser(tx, uid, false); err != nil {
			return err
		}

		// TODO: Disable all p2p subscriptions with the user.

		// Disable all subscriptions to topics where the user is the owner.
		if _, err = tx.Exec("UPDATE subscriptions LEFT JOIN topics ON subscriptions.topic=topics.name "+
			"SET subscriptions.updatedAt=$1, subscriptions.deletedAt=$2 WHERE topics.owner=$3",
			now, now, decoded_uid); err != nil {
			return err
		}
		// Disable all topics where the user is the owner.
		if _, err = tx.Exec("UPDATE topics SET updatedAt=$1, deletedAt=$2 WHERE owner=$3",
			now, now, decoded_uid); err != nil {
			return err
		}

		// Disable user.
		if _, err = tx.Exec("UPDATE users SET updatedAt=$1, deletedAt=$2 WHERE id=$3", now, now, decoded_uid); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (a *adapter) UserGetDisabled(since time.Time) ([]t.Uid, error) {
	rows, err := a.db.Queryx("SELECT id FROM users WHERE deletedat>=$1", since)
	if err != nil {
		return nil, err
	}

	var uids []t.Uid
	for rows.Next() {
		var userId int64
		if err = rows.Scan(&userId); err != nil {
			uids = nil
			break
		}
		uids = append(uids, store.EncodeUid(userId))
	}
	rows.Close()

	return uids, err
}

// UserUpdate updates user object.
func (a *adapter) UserUpdate(uid t.Uid, update map[string]interface{}) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	cols, args := updateByMap(update)
	decoded_uid := store.DecodeUid(uid)
	args = append(args, decoded_uid)

	_, err = tx.Exec("UPDATE users SET "+strings.Join(cols, ",")+" WHERE id=$1", args...)
	if err != nil {
		return err
	}

	// Tags are also stored in a separate table
	if tags := extractTags(update); tags != nil {
		// First delete all user tags
		_, err = tx.Exec("DELETE FROM usertags WHERE userid=$1", decoded_uid)
		if err != nil {
			return err
		}
		// Now insert new tags
		err = addTags(tx, "usertags", "userid", decoded_uid, tags, false)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// UserUpdateTags adds or resets user's tags
func (a *adapter) UserUpdateTags(uid t.Uid, add, remove, reset []string) ([]string, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	decoded_uid := store.DecodeUid(uid)

	if reset != nil {
		// Delete all tags first if resetting.
		_, err = tx.Exec("DELETE FROM usertags WHERE userid=$1", decoded_uid)
		if err != nil {
			return nil, err
		}
		add = reset
		remove = nil
	}

	// Now insert new tags. Ignore duplicates if resetting.
	err = addTags(tx, "usertags", "userid", decoded_uid, add, reset == nil)
	if err != nil {
		return nil, err
	}

	// Delete tags.
	err = removeTags(tx, "usertags", "userid", decoded_uid, remove)
	if err != nil {
		return nil, err
	}

	var allTags []string
	err = tx.Select(&allTags, "SELECT tag FROM usertags WHERE userid=$1", decoded_uid)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec("UPDATE users SET tags=$1 WHERE id=$2", t.StringSlice(allTags), decoded_uid)
	if err != nil {
		return nil, err
	}

	return allTags, tx.Commit()
}

// UserGetByCred returns user ID for the given validated credential.
func (a *adapter) UserGetByCred(method, value string) (t.Uid, error) {
	var decoded_uid int64
	err := a.db.Get(&decoded_uid, "SELECT userid FROM credentials WHERE synthetic=$1", method+":"+value)
	if err == nil {
		return store.EncodeUid(decoded_uid), nil
	}

	if err == sql.ErrNoRows {
		// Clear the error if user does not exist
		return t.ZeroUid, nil
	}
	return t.ZeroUid, err
}

// UserUnreadCount returns the total number of unread messages in all topics with
// the R permission.
func (a *adapter) UserUnreadCount(uid t.Uid) (int, error) {
	var count int
	err := a.db.Get(&count, "SELECT SUM(t.seqid)-SUM(s.readseqid) FROM topics AS t, subscriptions AS s "+
		"WHERE s.userid=$1 AND t.name=s.topic AND s.deletedat IS NULL AND t.deletedat IS NULL AND "+
		"INSTR(s.modewant, 'R')>0 AND INSTR(s.modegiven, 'R')>0", store.DecodeUid(uid))
	if err == nil {
		return count, nil
	}

	if err == sql.ErrNoRows {
		return 0, nil
	}

	return -1, err
}

// *****************************

func (a *adapter) topicCreate(tx *sqlx.Tx, topic *t.Topic) error {
	_, err := tx.Exec("INSERT INTO topics(createdAt,updatedAt,touchedAt,name,owner,access,public,tags) "+
		"VALUES($1,$2,$3,$4,$5,$6,$7,$8)",
		topic.CreatedAt, topic.UpdatedAt, topic.TouchedAt, topic.Id, store.DecodeUid(t.ParseUid(topic.Owner)),
		topic.Access, toJSON(topic.Public), topic.Tags)
	if err != nil {
		return err
	}

	// Save topic's tags to a separate table to make topic findable.
	return addTags(tx, "topictags", "topic", topic.Id, topic.Tags, false)
}

// TopicCreate saves topic object to database.
func (a *adapter) TopicCreate(topic *t.Topic) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	err = a.topicCreate(tx, topic)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// If undelete = true - update subscription on duplicate key, otherwise ignore the duplicate.
func createSubscription(tx *sqlx.Tx, sub *t.Subscription, undelete bool) error {

	isOwner := (sub.ModeGiven & sub.ModeWant).IsOwner()

	jpriv := toJSON(sub.Private)
	decoded_uid := store.DecodeUid(t.ParseUid(sub.User))
	_, err := tx.Exec(
		"INSERT INTO subscriptions(createdAt,updatedAt,deletedAt,userid,topic,modeWant,modeGiven,private) "+
			"VALUES($1,$2,NULL,$3,$4,$5,$6,$7)",
		sub.CreatedAt, sub.UpdatedAt, decoded_uid, sub.Topic, sub.ModeWant.String(), sub.ModeGiven.String(), jpriv)

	if err != nil && isDupe(err) {
		if undelete {
			_, err = tx.Exec("UPDATE subscriptions SET createdAt=$1,updatedAt=$2,deletedAt=NULL,modeGiven=$3 "+
				"WHERE topic=$4 AND userid=$5",
				sub.CreatedAt, sub.UpdatedAt, sub.ModeGiven.String(), sub.Topic, decoded_uid)

		} else {
			_, err = tx.Exec(
				"UPDATE subscriptions SET createdAt=$1,updatedAt=$2,deletedAt=NULL,modeWant=$3,modeGiven=$4,private=$5 "+
					"WHERE topic=$6 AND userid=$7",
				sub.CreatedAt, sub.UpdatedAt, sub.ModeWant.String(), sub.ModeGiven.String(), jpriv, sub.Topic, decoded_uid)
		}
	}
	if err == nil && isOwner {
		_, err = tx.Exec("UPDATE topics SET owner=$1 WHERE name=$2", decoded_uid, sub.Topic)
	}
	return err
}

// TopicCreateP2P given two users creates a p2p topic
func (a *adapter) TopicCreateP2P(initiator, invited *t.Subscription) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	err = createSubscription(tx, initiator, false)
	if err != nil {
		return err
	}

	err = createSubscription(tx, invited, true)
	if err != nil {
		return err
	}

	topic := &t.Topic{ObjHeader: t.ObjHeader{Id: initiator.Topic}}
	topic.ObjHeader.MergeTimes(&initiator.ObjHeader)
	topic.TouchedAt = initiator.GetTouchedAt()
	err = a.topicCreate(tx, topic)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// TopicGet loads a single topic by name, if it exists. If the topic does not exist the call returns (nil, nil)
func (a *adapter) TopicGet(topic string) (*t.Topic, error) {
	// Fetch topic by name
	var tt = new(t.Topic)
	err := a.db.Get(tt,
		"SELECT createdat,updatedat,deletedat,touchedat,name AS id,access,owner,seqid,delid,public,tags FROM topics WHERE name=$1",
		topic)

	if err != nil {
		if err == sql.ErrNoRows {
			// Nothing found - clear the error
			err = nil
		}
		return nil, err
	}

	tt.Owner = encodeUidString(tt.Owner).String()
	tt.Public = fromJSON(tt.Public)

	return tt, nil
}

// TopicsForUser loads user's contact list: p2p and grp topics, except for 'me' & 'fnd' subscriptions.
// Reads and denormalizes Public value.
func (a *adapter) TopicsForUser(uid t.Uid, keepDeleted bool, opts *t.QueryOpt) ([]t.Subscription, error) {
	// Fetch user's subscriptions
	queryVar := 1
	q := `SELECT createdat,updatedat,deletedat,topic,delid,recvseqid,
		readseqid,modewant,modegiven,private FROM subscriptions WHERE userid=$` + strconv.Itoa(queryVar)
	args := []interface{}{store.DecodeUid(uid)}
	if !keepDeleted {
		// Filter out rows with defined DeletedAt
		q += " AND deletedat IS NULL"
	}
	queryVar++

	limit := a.maxResults
	if opts != nil {
		// Ignore IfModifiedSince - we must return all entries
		// Those unmodified will be stripped of Public & Private.

		if opts.Topic != "" {
			q += " AND topic=$" + strconv.Itoa(queryVar)
			args = append(args, opts.Topic)
			queryVar++
		}
		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	q += " LIMIT $" + strconv.Itoa(queryVar)
	args = append(args, limit)

	rows, err := a.db.Queryx(q, args...)
	if err != nil {
		return nil, err
	}

	// Fetch subscriptions. Two queries are needed: users table (me & p2p) and topics table (p2p and grp).
	// Prepare a list of Separate subscriptions to users vs topics
	var sub t.Subscription
	join := make(map[string]t.Subscription) // Keeping these to make a join with table for .private and .access
	topq := make([]interface{}, 0, 16)
	usrq := make([]interface{}, 0, 16)
	for rows.Next() {
		if err = rows.StructScan(&sub); err != nil {
			break
		}

		sub.User = uid.String()
		tcat := t.GetTopicCat(sub.Topic)

		// 'me' or 'fnd' subscription, skip
		if tcat == t.TopicCatMe || tcat == t.TopicCatFnd {
			continue

			// p2p subscription, find the other user to get user.Public
		} else if tcat == t.TopicCatP2P {
			uid1, uid2, _ := t.ParseP2P(sub.Topic)
			if uid1 == uid {
				usrq = append(usrq, store.DecodeUid(uid2))
			} else {
				usrq = append(usrq, store.DecodeUid(uid1))
			}
			topq = append(topq, sub.Topic)

			// grp subscription
		} else {
			topq = append(topq, sub.Topic)
		}
		sub.Private = fromJSON(sub.Private)
		join[sub.Topic] = sub
	}
	rows.Close()

	if err != nil {
		return nil, err
	}

	var subs []t.Subscription
	if len(topq) > 0 || len(usrq) > 0 {
		subs = make([]t.Subscription, 0, len(join))
	}

	if len(topq) > 0 {
		// Fetch grp & p2p topics
		q, topq, _ := sqlx.In(
			"SELECT createdat,updatedat,deletedat,touchedat,name AS id,access,seqid,delid,public,tags "+
				"FROM topics WHERE name IN ($1)", topq)
		q = a.db.Rebind(q)
		rows, err = a.db.Queryx(q, topq...)
		if err != nil {
			return nil, err
		}

		var top t.Topic
		for rows.Next() {
			if err = rows.StructScan(&top); err != nil {
				break
			}

			sub = join[top.Id]
			sub.ObjHeader.MergeTimes(&top.ObjHeader)
			sub.SetTouchedAt(top.TouchedAt)
			sub.SetSeqId(top.SeqId)
			if t.GetTopicCat(sub.Topic) == t.TopicCatGrp {
				// all done with a grp topic
				sub.SetPublic(fromJSON(top.Public))
				subs = append(subs, sub)
			} else {
				// put back the updated value of a p2p subsription, will process further below
				join[top.Id] = sub
			}
		}
		rows.Close()
	}

	// Fetch p2p users and join to p2p tables
	if err == nil && len(usrq) > 0 {
		q, usrq, _ := sqlx.In(
			"SELECT id,state,createdat,updatedat,deletedat,access,lastseen,useragent,public,tags FROM users WHERE id IN ($1)",
			usrq)
		rows, err = a.db.Queryx(q, usrq...)
		if err != nil {
			return nil, err
		}

		var usr t.User
		for rows.Next() {
			if err = rows.StructScan(&usr); err != nil {
				break
			}

			// Optionally skip deleted users.
			if usr.DeletedAt != nil && !keepDeleted {
				continue
			}

			uid2 := encodeUidString(usr.Id)
			if sub, ok := join[uid.P2PName(uid2)]; ok {
				sub.ObjHeader.MergeTimes(&usr.ObjHeader)
				sub.SetPublic(fromJSON(usr.Public))
				sub.SetWith(uid2.UserId())
				sub.SetDefaultAccess(usr.Access.Auth, usr.Access.Anon)
				sub.SetLastSeenAndUA(usr.LastSeen, usr.UserAgent)
				subs = append(subs, sub)
			}
		}
		rows.Close()
	}
	return subs, err
}

// UsersForTopic loads users subscribed to the given topic.
// The difference between UsersForTopic vs SubsForTopic is that the former loads user.public,
// the latter does not.
func (a *adapter) UsersForTopic(topic string, keepDeleted bool, opts *t.QueryOpt) ([]t.Subscription, error) {
	tcat := t.GetTopicCat(topic)

	queryVar := 1
	// Fetch all subscribed users. The number of users is not large
	q := `SELECT s.createdat,s.updatedat,s.deletedat,s.userid,s.topic,s.delid,s.recvseqid,
		s.readseqid,s.modewant,s.modegiven,u.public,s.private
		FROM subscriptions AS s JOIN users AS u ON s.userid=u.id
		WHERE s.topic=$1`
	args := []interface{}{topic}
	if !keepDeleted {
		// Filter out rows with users deleted
		q += " AND u.deletedat IS NULL"

		// For p2p topics we must load all subscriptions including deleted.
		// Otherwise it will be impossible to swipe Public values.
		if tcat != t.TopicCatP2P {
			// Filter out deletd subscriptions.
			q += " AND s.deletedAt IS NULL"
		}
	}

	limit := a.maxResults
	var oneUser t.Uid
	if opts != nil {
		// Ignore IfModifiedSince - we must return all entries
		// Those unmodified will be stripped of Public & Private.

		if !opts.User.IsZero() {
			// For p2p topics we have to fetch both users otherwise public cannot be swapped.
			if tcat != t.TopicCatP2P {
				queryVar++
				q += " AND s.userid=$" + strconv.Itoa(queryVar)
				args = append(args, store.DecodeUid(opts.User))
			}
			oneUser = opts.User
		}
		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}
	queryVar++
	q += " LIMIT $" + strconv.Itoa(queryVar)
	args = append(args, limit)

	rows, err := a.db.Queryx(q, args...)
	if err != nil {
		return nil, err
	}

	// Fetch subscriptions
	var sub t.Subscription
	var subs []t.Subscription
	var public interface{}
	for rows.Next() {
		if err = rows.Scan(
			&sub.CreatedAt, &sub.UpdatedAt, &sub.DeletedAt,
			&sub.User, &sub.Topic, &sub.DelId, &sub.RecvSeqId,
			&sub.ReadSeqId, &sub.ModeWant, &sub.ModeGiven,
			&public, &sub.Private); err != nil {
			break
		}

		sub.User = encodeUidString(sub.User).String()
		sub.Private = fromJSON(sub.Private)
		sub.SetPublic(fromJSON(public))
		subs = append(subs, sub)
	}
	rows.Close()

	if err == nil && tcat == t.TopicCatP2P && len(subs) > 0 {
		// Swap public values of P2P topics as expected.
		if len(subs) == 1 {
			// The other user is deleted, nothing we can do.
			subs[0].SetPublic(nil)
		} else {
			pub := subs[0].GetPublic()
			subs[0].SetPublic(subs[1].GetPublic())
			subs[1].SetPublic(pub)
		}

		// Remove deleted and unneeded subscriptions
		if !keepDeleted || !oneUser.IsZero() {
			var xsubs []t.Subscription
			for i := range subs {
				if (subs[i].DeletedAt != nil && !keepDeleted) || (!oneUser.IsZero() && subs[i].Uid() != oneUser) {
					continue
				}
				xsubs = append(xsubs, subs[i])
			}
			subs = xsubs
		}
	}

	return subs, err
}

// OwnTopics loads a slice of topic names where the user is the owner.
func (a *adapter) OwnTopics(uid t.Uid, opts *t.QueryOpt) ([]string, error) {
	rows, err := a.db.Queryx("SELECT name FROM topics WHERE owner=$1", store.DecodeUid(uid))
	if err != nil {
		return nil, err
	}

	var names []string
	var name string
	for rows.Next() {
		if err = rows.Scan(&name); err != nil {
			break
		}
		names = append(names, name)
	}
	rows.Close()

	return names, err
}

func (a *adapter) TopicShare(shares []*t.Subscription) (int, error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	for _, sub := range shares {
		err = createSubscription(tx, sub, true)
		if err != nil {
			return 0, err
		}
	}

	return len(shares), tx.Commit()
}

// TopicDelete deletes specified topic.
func (a *adapter) TopicDelete(topic string, hard bool) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if hard {
		if _, err = tx.Exec("DELETE FROM subscriptions WHERE topic=$1", topic); err != nil {
			return err
		}

		if err = messageDeleteList(tx, topic, nil); err != nil {
			return err
		}

		if _, err = tx.Exec("DELETE FROM topictags WHERE topic=$1", topic); err != nil {
			return err
		}

		if _, err = tx.Exec("DELETE FROM topics WHERE name=$1", topic); err != nil {
			return err
		}
	} else {
		now := t.TimeNow()
		if _, err = tx.Exec("UPDATE subscriptions SET updatedat=$1,deletedat=$2 WHERE topic=$3", now, now, topic); err != nil {
			return err
		}

		if _, err = tx.Exec("UPDATE topics SET updatedat=$1,deletedat=$2 WHERE name=$3", now, now, topic); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *adapter) TopicUpdateOnMessage(topic string, msg *t.Message) error {
	_, err := a.db.Exec("UPDATE topics SET seqid=$1,touchedat=$2 WHERE name=$3", msg.SeqId, msg.CreatedAt, topic)

	return err
}

func (a *adapter) TopicUpdate(topic string, update map[string]interface{}) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	cols, args := updateByMap(update)
	args = append(args, topic)
	_, err = tx.Exec("UPDATE topics SET "+strings.Join(cols, ",")+" WHERE name=$1", args...)
	if err != nil {
		return err
	}

	// Tags are also stored in a separate table
	if tags := extractTags(update); tags != nil {
		// First delete all user tags
		_, err = tx.Exec("DELETE FROM topictags WHERE topic=$1", topic)
		if err != nil {
			return err
		}
		// Now insert new tags
		err = addTags(tx, "topictags", "topic", topic, tags, false)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (a *adapter) TopicOwnerChange(topic string, newOwner, oldOwner t.Uid) error {
	_, err := a.db.Exec("UPDATE topics SET owner=$1 WHERE name=$2", store.DecodeUid(newOwner), topic)
	return err
}

// Get a subscription of a user to a topic
func (a *adapter) SubscriptionGet(topic string, user t.Uid) (*t.Subscription, error) {
	var sub t.Subscription
	err := a.db.Get(&sub, `SELECT createdat,updatedat,deletedat,userid AS user,topic,delid,recvseqid,
		readseqid,modewant,modegiven,private FROM subscriptions WHERE topic=$1 AND userid=$2`,
		topic, store.DecodeUid(user))

	if err != nil {
		if err == sql.ErrNoRows {
			// Nothing found - clear the error
			err = nil
		}
		return nil, err
	}

	if sub.DeletedAt != nil {
		return nil, nil
	}

	sub.Private = fromJSON(sub.Private)

	return &sub, nil
}

// Update time when the user was last attached to the topic
func (a *adapter) SubsLastSeen(topic string, user t.Uid, lastSeen map[string]time.Time) error {
	_, err := a.db.Exec("UPDATE subscriptions SET lastseen=$1,useragent=$2 WHERE topic=$3 AND userid=$4",
		lastSeen["LastSeen"], lastSeen["UserAgent"], topic, store.DecodeUid(user))

	return err
}

// SubsForUser loads a list of user's subscriptions to topics. Does NOT load Public value.
// TODO: this is used only for presence notifications, no need to load Private either.
func (a *adapter) SubsForUser(forUser t.Uid, keepDeleted bool, opts *t.QueryOpt) ([]t.Subscription, error) {
	queryVar := 1
	q := `SELECT createdat,updatedat,deletedat,userid AS user,topic,delid,recvseqid,
		readseqid,modewant,modegiven,private FROM subscriptions WHERE userid=$1`
	queryVar++
	args := []interface{}{store.DecodeUid(forUser)}

	if !keepDeleted {
		q += " AND deletedAt IS NULL"
	}

	limit := a.maxResults // maxResults here, not maxSubscribers
	if opts != nil {
		// Ignore IfModifiedSince - we must return all entries
		// Those unmodified will be stripped of Public & Private.

		if opts.Topic != "" {
			q += " AND topic=$" + strconv.Itoa(queryVar)
			args = append(args, opts.Topic)
			queryVar++
		}
		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}
	q += " LIMIT $" + strconv.Itoa(queryVar)
	args = append(args, limit)

	rows, err := a.db.Queryx(q, args...)
	if err != nil {
		return nil, err
	}

	var subs []t.Subscription
	var ss t.Subscription
	for rows.Next() {
		if err = rows.StructScan(&ss); err != nil {
			break
		}
		ss.User = forUser.String()
		ss.Private = fromJSON(ss.Private)
		subs = append(subs, ss)
	}
	rows.Close()

	return subs, err
}

// SubsForTopic fetches all subsciptions for a topic. Does NOT load Public value.
// The difference between UsersForTopic vs SubsForTopic is that the former loads user.public,
// the latter does not.
func (a *adapter) SubsForTopic(topic string, keepDeleted bool, opts *t.QueryOpt) ([]t.Subscription, error) {
	queryVar := 1
	q := `SELECT createdat,updatedat,deletedat,userid AS user,topic,delid,recvseqid,
		readseqid,modewant,modegiven,private FROM subscriptions WHERE topic=$1`
	args := []interface{}{topic}
	if !keepDeleted {
		// Filter out rows where DeletedAt is defined
		q += " AND deletedAt IS NULL"
	}
	limit := a.maxResults
	if opts != nil {
		// Ignore IfModifiedSince - we must return all entries
		// Those unmodified will be stripped of Public & Private.

		if !opts.User.IsZero() {
			queryVar++
			q += " AND userid=$" + strconv.Itoa(queryVar)
			args = append(args, store.DecodeUid(opts.User))
		}
		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	queryVar++
	q += " LIMIT $" + strconv.Itoa(queryVar)
	args = append(args, limit)

	rows, err := a.db.Queryx(q, args...)
	if err != nil {
		return nil, err
	}

	var subs []t.Subscription
	var ss t.Subscription
	for rows.Next() {
		if err = rows.StructScan(&ss); err != nil {
			break
		}

		ss.User = encodeUidString(ss.User).String()
		ss.Private = fromJSON(ss.Private)
		subs = append(subs, ss)
	}
	rows.Close()

	return subs, err
}

// SubsUpdate updates one or multiple subscriptions to a topic.
func (a *adapter) SubsUpdate(topic string, user t.Uid, update map[string]interface{}) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	cols, args := updateByMap(update)
	updateLen := len(update)
	q := "UPDATE subscriptions SET " + strings.Join(cols, ",") + " WHERE topic=$" + strconv.Itoa(updateLen+1)
	args = append(args, topic)
	if !user.IsZero() {
		// Update just one topic subscription
		q += " AND userid=$" + strconv.Itoa(updateLen+2)
		args = append(args, store.DecodeUid(user))
	}

	if _, err = tx.Exec(q, args...); err != nil {
		return err
	}

	return tx.Commit()
}

// SubsDelete marks subscription as deleted.
func (a *adapter) SubsDelete(topic string, user t.Uid) error {
	now := t.TimeNow()
	res, err := a.db.Exec("UPDATE subscriptions SET updatedat=$1, deletedat=$2 WHERE topic=$3 AND userid=$4 AND deletedat IS NULL",
		now, now, topic, store.DecodeUid(user))
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err == nil && affected == 0 {
		err = t.ErrNotFound
	}
	return err
}

// SubsDelForTopic marks all subscriptions to the given topic as deleted
func (a *adapter) SubsDelForTopic(topic string, hard bool) error {
	var err error
	if hard {
		_, err = a.db.Exec("DELETE FROM subscriptions WHERE topic=$1", topic)
	} else {
		now := t.TimeNow()
		_, err = a.db.Exec("UPDATE subscriptions SET updatedat=$1, deletedat=$2 WHERE topic=$3 AND deletedat IS NULL",
			now, now, topic)
	}
	return err
}

// subsDelForTopic marks user's subscriptions as deleted
func subsDelForUser(tx *sqlx.Tx, user t.Uid, hard bool) error {
	var err error
	if hard {
		_, err = tx.Exec("DELETE FROM subscriptions WHERE userid=$1", store.DecodeUid(user))
	} else {
		now := t.TimeNow()
		_, err = tx.Exec("UPDATE subscriptions SET updatedat=$1, deletedat=$2 WHERE userid=$3",
			now, now, store.DecodeUid(user))
	}
	return err
}

// SubsDelForTopic marks user's subscriptions as deleted
func (a *adapter) SubsDelForUser(user t.Uid, hard bool) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if err = subsDelForUser(tx, user, hard); err != nil {
		return err
	}

	return tx.Commit()

}

// Returns a list of users who match given tags, such as "email:jdoe@example.com" or "tel:+18003287448".
// Searching the 'users.Tags' for the given tags using respective index.
func (a *adapter) FindUsers(uid t.Uid, req, opt []string) ([]t.Subscription, error) {
	index := make(map[string]struct{})
	var args []interface{}
	for _, tag := range append(req, opt...) {
		args = append(args, tag)
		index[tag] = struct{}{}
	}

	query := "SELECT u.id,u.createdat,u.updatedat,u.access,u.public,u.tags,COUNT(*) AS matches " +
		"FROM users AS u LEFT JOIN usertags AS t ON t.userid=u.id " +
		"WHERE t.tag IN ("
	queryVar := len(req) + len(opt)
	for i := 1; i <= queryVar; i++ {
		query += "$" + strconv.Itoa(i) + ","
	}

	query = strings.TrimRight(query, ",")
	query += ") AND u.deletedat IS NULL " +
		"GROUP BY u.id,u.createdat,u.updatedat,u.public,u.tags "
	if len(req) > 0 {
		query += "HAVING COUNT(t.tag IN ("
		for _, tag := range req {
			queryVar++
			query += "$" + strconv.Itoa(queryVar) + ","
			args = append(args, tag)
		}
		query = strings.TrimRight(query, ",")
		queryVar++
		query += ") OR NULL)>=$" + strconv.Itoa(queryVar)
		args = append(args, len(req))
	}
	queryVar++
	query += "ORDER BY matches DESC LIMIT $" + strconv.Itoa(queryVar)

	// Get users matched by tags, sort by number of matches from high to low.
	rows, err := a.db.Queryx(query, append(args, a.maxResults)...)

	if err != nil {
		return nil, err
	}

	var userId int64
	var public interface{}
	var access t.DefaultAccess
	var userTags t.StringSlice
	var ignored int
	var sub t.Subscription
	var subs []t.Subscription
	thisUser := store.DecodeUid(uid)
	for rows.Next() {
		if err = rows.Scan(&userId, &sub.CreatedAt, &sub.UpdatedAt, &access, &public, &userTags, &ignored); err != nil {
			subs = nil
			break
		}

		if userId == thisUser {
			// Skip the callee
			continue
		}
		sub.User = store.EncodeUid(userId).String()
		sub.SetPublic(fromJSON(public))
		sub.SetDefaultAccess(access.Auth, access.Anon)
		foundTags := make([]string, 0, 1)
		for _, tag := range userTags {
			if _, ok := index[tag]; ok {
				foundTags = append(foundTags, tag)
			}
		}
		sub.Private = foundTags
		subs = append(subs, sub)
	}
	rows.Close()

	return subs, err

}

// Returns a list of topics with matching tags.
// Searching the 'topics.Tags' for the given tags using respective index.
func (a *adapter) FindTopics(req, opt []string) ([]t.Subscription, error) {
	index := make(map[string]struct{})
	var args []interface{}
	for _, tag := range append(req, opt...) {
		args = append(args, tag)
		index[tag] = struct{}{}
	}

	query := "SELECT t.name AS topic,t.createdat,t.updatedat,t.access,t.public,t.tags,COUNT(*) AS matches " +
		"FROM topics AS t LEFT JOIN topictags AS tt ON t.name=tt.topic " +
		"WHERE tt.tag IN ("

	queryVar := len(req) + len(opt)
	for i := 1; i <= queryVar; i++ {
		query += "$" + strconv.Itoa(i) + ","
	}

	query = strings.TrimRight(query, ",")
	query += ") AND t.deletedat IS NULL " +
		"GROUP BY t.name,t.createdat,t.updatedat,t.public,t.tags "

	if len(req) > 0 {
		query += "HAVING COUNT(tt.tag IN ("
		for _, tag := range append(req) {
			queryVar++
			query += "$" + strconv.Itoa(queryVar) + ","
			args = append(args, tag)
		}

		query = strings.TrimRight(query, ",")
		queryVar++
		query += ") OR NULL)>= $" + strconv.Itoa(queryVar)
		args = append(args, len(req))
	}
	queryVar++
	query += "ORDER BY matches DESC LIMIT $" + strconv.Itoa(queryVar)
	rows, err := a.db.Queryx(query, append(args, a.maxResults)...)

	if err != nil {
		return nil, err
	}

	var access t.DefaultAccess
	var public interface{}
	var topicTags t.StringSlice
	var ignored int
	var sub t.Subscription
	var subs []t.Subscription
	for rows.Next() {
		if err = rows.Scan(&sub.Topic, &sub.CreatedAt, &sub.UpdatedAt, &access, &public, &topicTags, &ignored); err != nil {
			subs = nil
			break
		}

		sub.SetPublic(fromJSON(public))
		sub.SetDefaultAccess(access.Auth, access.Anon)
		foundTags := make([]string, 0, 1)
		for _, tag := range topicTags {
			if _, ok := index[tag]; ok {
				foundTags = append(foundTags, tag)
			}
		}
		sub.Private = foundTags
		subs = append(subs, sub)
	}
	rows.Close()

	if err != nil {
		return nil, err
	}
	return subs, nil

}

// Messages
func (a *adapter) MessageSave(msg *t.Message) error {
	res, err := a.db.Exec(`INSERT INTO messages(createdAt,updatedAt,seqid,topic,"from",head,content)
  VALUES($1,$2,$3,$4,$5,$6,$7)`,
		msg.CreatedAt, msg.UpdatedAt, msg.SeqId, msg.Topic,
		store.DecodeUid(t.ParseUid(msg.From)), msg.Head, toJSON(msg.Content))
	if err == nil {
		id, _ := res.LastInsertId()
		msg.SetUid(t.Uid(id))
	}
	return err
}

func (a *adapter) MessageGetAll(topic string, forUser t.Uid, opts *t.QueryOpt) ([]t.Message, error) {
	var limit = a.maxResults
	var lower = 0
	var upper = 1 << 31

	if opts != nil {
		if opts.Since > 0 {
			lower = opts.Since
		}
		if opts.Before > 0 {
			// PostgreSql BETWEEN is inclusive-inclusive, Tinode API requires inclusive-exclusive, thus -1
			upper = opts.Before - 1
		}

		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	unum := store.DecodeUid(forUser)
	rows, err := a.db.Queryx(
		"SELECT m.createdat,m.updatedat,m.deletedat,m.delid,m.seqid,m.topic,m.`from`,m.head,m.content"+
			" FROM messages AS m LEFT JOIN dellog AS d"+
			" ON d.topic=m.topic AND m.seqid BETWEEN d.low AND d.hi-1 AND d.deletedfor=$1"+
			" WHERE m.delid=0 AND m.topic=$2 AND m.seqid BETWEEN $3 AND $4 AND d.deletedfor IS NULL"+
			" ORDER BY m.seqid DESC LIMIT $5",
		unum, topic, lower, upper, limit)

	if err != nil {
		return nil, err
	}

	var msgs []t.Message
	for rows.Next() {
		var msg t.Message
		if err = rows.StructScan(&msg); err != nil {
			break
		}
		msg.From = encodeUidString(msg.From).String()
		msg.Content = fromJSON(msg.Content)
		msgs = append(msgs, msg)
	}
	rows.Close()
	return msgs, err
}

var dellog struct {
	Topic      string
	Deletedfor int64
	Delid      int
	Low        int
	Hi         int
}

// Get ranges of deleted messages
func (a *adapter) MessageGetDeleted(topic string, forUser t.Uid, opts *t.QueryOpt) ([]t.DelMessage, error) {
	var limit = a.maxResults
	var lower = 0
	var upper = 1 << 31

	if opts != nil {
		if opts.Since > 0 {
			lower = opts.Since
		}
		if opts.Before > 1 {
			// DelRange is inclusive-exclusive, while BETWEEN is inclusive-inclisive.
			upper = opts.Before - 1
		}

		if opts.Limit > 0 && opts.Limit < limit {
			limit = opts.Limit
		}
	}

	// Fetch log of deletions
	rows, err := a.db.Queryx("SELECT topic,deletedfor,delid,low,hi FROM dellog WHERE topic=$1 AND delid BETWEEN $2 AND $3"+
		" AND (deletedFor=0 OR deletedFor=$4)"+
		" ORDER BY delid LIMIT $5", topic, lower, upper, store.DecodeUid(forUser), limit)
	if err != nil {
		return nil, err
	}

	var dmsgs []t.DelMessage
	var dmsg t.DelMessage
	for rows.Next() {
		if err = rows.StructScan(&dellog); err != nil {
			dmsgs = nil
			break
		}
		if dellog.Delid != dmsg.DelId {
			if dmsg.DelId > 0 {
				dmsgs = append(dmsgs, dmsg)
			}
			dmsg.DelId = dellog.Delid
			dmsg.Topic = dellog.Topic
			if dellog.Deletedfor > 0 {
				dmsg.DeletedFor = store.EncodeUid(dellog.Deletedfor).String()
			}
			if dmsg.SeqIdRanges == nil {
				dmsg.SeqIdRanges = []t.Range{}
			}
		}
		if dellog.Hi <= dellog.Low+1 {
			dellog.Hi = 0
		}
		dmsg.SeqIdRanges = append(dmsg.SeqIdRanges, t.Range{dellog.Low, dellog.Hi})
	}
	if dmsg.DelId > 0 {
		dmsgs = append(dmsgs, dmsg)
	}
	rows.Close()

	return dmsgs, err
}

func messageDeleteList(tx *sqlx.Tx, topic string, toDel *t.DelMessage) error {
	var err error
	if toDel == nil {
		// Whole topic is being deleted, thus also deleting all messages.
		_, err = tx.Exec("DELETE FROM dellog WHERE topic=$1", topic)
		if err == nil {
			_, err = tx.Exec("DELETE FROM messages WHERE topic=$2", topic)
		}
		// filemsglinks will be deleted because of ON DELETE CASCADE

	} else {
		// Only some messages are being deleted
		// Start with making log entries
		forUser := decodeUidString(toDel.DeletedFor)
		var insert *sql.Stmt
		if insert, err = tx.Prepare(
			"INSERT INTO dellog(topic,deletedfor,delid,low,hi) VALUES($1,$2,$3,$4,$5)"); err != nil {
			return err
		}

		// Counter of deleted messages
		seqCount := 0
		for _, rng := range toDel.SeqIdRanges {
			if rng.Hi == 0 {
				// Dellog must contain valid Low and *Hi*.
				rng.Hi = rng.Low + 1
			}
			seqCount += rng.Hi - rng.Low
			if _, err = insert.Exec(topic, forUser, toDel.DelId, rng.Low, rng.Hi); err != nil {
				break
			}
		}

		queryVar := 0
		if err == nil && toDel.DeletedFor == "" {
			// Hard-deleting messages requires updates to the messages table
			where := "m.topic=$1 AND "
			args := []interface{}{topic}
			if len(toDel.SeqIdRanges) > 1 || toDel.SeqIdRanges[0].Hi == 0 {
				for _, r := range toDel.SeqIdRanges {
					if r.Hi == 0 {
						args = append(args, r.Low)
					} else {
						for i := r.Low; i < r.Hi; i++ {
							args = append(args, i)
						}
					}
				}

				where += "m.seqid IN ("
				for i := 0; i < seqCount; i++ {
					queryVar++
					where += "$" + strconv.Itoa(queryVar) + ","
				}
				where = strings.TrimRight(where, ",")
				where += ")"
			} else {
				// Optimizing for a special case of single range low..hi.

				where += "m.seqid BETWEEN $2 AND $3"
				// MySQL's BETWEEN is inclusive-inclusive thus decrement Hi by 1.
				args = append(args, toDel.SeqIdRanges[0].Low, toDel.SeqIdRanges[0].Hi-1)
				queryVar += 2
			}
			where += " AND m.deletedAt IS NULL"

			_, err = tx.Exec("DELETE fml.* FROM filemsglinks AS fml INNER JOIN messages AS m ON m.id=fml.msgid WHERE "+
				where, args...)
			if err != nil {
				return err
			}

			for i := 1; i <= queryVar; i++ {
				where = strings.Replace(where, "$"+strconv.Itoa(i), "$"+strconv.Itoa(i+2), 1)
			}

			_, err = tx.Exec("UPDATE messages AS m SET m.deletedAt=$1,m.delId=$2,m.head=NULL,m.content=NULL WHERE "+
				where,
				append([]interface{}{t.TimeNow(), toDel.DelId}, args...)...)
		}
	}

	return err
}

// MessageDeleteList deletes messages in the given topic with seqIds from the list
func (a *adapter) MessageDeleteList(topic string, toDel *t.DelMessage) (err error) {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if err = messageDeleteList(tx, topic, toDel); err != nil {
		return err
	}

	return tx.Commit()
}

// MessageAttachments connects given message to a list of file record IDs.
func (a *adapter) MessageAttachments(msgId t.Uid, fids []string) error {
	var args []interface{}
	var values []string
	strNow := t.TimeNow().Format("2006-01-02T15:04:05.999")
	// createdat,fileid,msgid
	val := "VALUES('" + strNow + "',$1," + strconv.FormatInt(int64(msgId), 10) + ")"
	for _, fid := range fids {
		id := t.ParseUid(fid)
		if id.IsZero() {
			return t.ErrMalformed
		}
		values = append(values, val)
		args = append(args, store.DecodeUid(id))
	}
	if len(args) == 0 {
		return t.ErrMalformed
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	_, err = a.db.Exec("INSERT INTO filemsglinks(createdat,fileid,msgid) "+strings.Join(values, ","), args...)
	if err != nil {
		return err
	}

	query := "UPDATE fileuploads SET updatedat='" + strNow + "' WHERE id IN ("

	for i := 1; i <= len(args); i++ {
		query += "$" + strconv.Itoa(i) + ","
	}
	query = strings.TrimRight(query, ",")

	_, err = tx.Exec(query, args...)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func deviceHasher(deviceID string) string {
	// Generate custom key as [64-bit hash of device id] to ensure predictable
	// length of the key
	hasher := fnv.New64()
	hasher.Write([]byte(deviceID))
	return strconv.FormatUint(uint64(hasher.Sum64()), 16)
}

// Device management for push notifications
func (a *adapter) DeviceUpsert(uid t.Uid, def *t.DeviceDef) error {
	hash := deviceHasher(def.DeviceId)

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Ensure uniqueness of the device ID: delete all records of the device ID
	_, err = tx.Exec("DELETE FROM devices WHERE hash=$1", hash)
	if err != nil {
		return err
	}

	// Actually add/update DeviceId for the new user
	_, err = tx.Exec("INSERT INTO devices(userid, hash, deviceId, platform, lastseen, lang) VALUES($1,$2,$3,$4,$5,$6)",
		store.DecodeUid(uid), hash, def.DeviceId, def.Platform, def.LastSeen, def.Lang)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (a *adapter) DeviceGetAll(uids ...t.Uid) (map[t.Uid][]t.DeviceDef, int, error) {
	var unums []interface{}
	for _, uid := range uids {
		unums = append(unums, store.DecodeUid(uid))
	}

	q, unums, _ := sqlx.In("SELECT userid,deviceid,platform,lastseen,lang FROM devices WHERE userid IN ($1)", unums)
	rows, err := a.db.Queryx(q, unums...)
	if err != nil {
		return nil, 0, err
	}

	var device struct {
		Userid   int64
		Deviceid string
		Platform string
		Lastseen time.Time
		Lang     string
	}

	result := make(map[t.Uid][]t.DeviceDef)
	count := 0
	for rows.Next() {
		if err = rows.StructScan(&device); err != nil {
			break
		}
		uid := store.EncodeUid(device.Userid)
		udev := result[uid]
		udev = append(udev, t.DeviceDef{
			DeviceId: device.Deviceid,
			Platform: device.Platform,
			LastSeen: device.Lastseen,
			Lang:     device.Lang,
		})
		result[uid] = udev
		count++
	}
	rows.Close()

	return result, count, err
}

func deviceDelete(tx *sqlx.Tx, uid t.Uid, deviceID string) error {
	var err error
	if deviceID == "" {
		_, err = tx.Exec("DELETE FROM devices WHERE userid=$1", store.DecodeUid(uid))
	} else {
		_, err = tx.Exec("DELETE FROM devices WHERE userid=$1 AND hash=$2", store.DecodeUid(uid), deviceHasher(deviceID))
	}
	return err
}

func (a *adapter) DeviceDelete(uid t.Uid, deviceID string) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	err = deviceDelete(tx, uid, deviceID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// Credential management

// CredUpsert adds or updates a validation record. Returns true if inserted, false if updated.
// 1. if credential is validated:
// 1.1 Hard-delete unconfirmed equivalent record, if exists.
// 1.2 Insert new. Report error if duplicate.
// 2. if credential is not validated:
// 2.1 Check if validated equivalent exist. If so, report an error.
// 2.2 Soft-delete all unvalidated records of the same method.
// 2.3 Undelete existing credential. Return if successful.
// 2.4 Insert new credential record.
func (a *adapter) CredUpsert(cred *t.Credential) (bool, error) {
	var err error

	tx, err := a.db.Beginx()
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			log.Println("Rollback")
			tx.Rollback()
		}
	}()

	now := t.TimeNow()
	userId := decodeUidString(cred.User)

	// Enforce uniqueness: if credential is confirmed, "method:value" must be unique.
	// if credential is not yet confirmed, "userid:method:value" is unique.
	synth := cred.Method + ":" + cred.Value

	if !cred.Done {
		// Check if this credential is already validated.
		var done bool
		err = tx.Get(&done, "SELECT done FROM credentials WHERE synthetic=$1", synth)
		if err == nil {
			return false, t.ErrDuplicate
		}
		if err != sql.ErrNoRows {
			return false, err
		}
		// We are going to insert new record.
		synth = cred.User + ":" + synth

		// Adding new unvalidated credential. Deactivate all unvalidated records of this user and method.
		_, err = tx.Exec("UPDATE credentials SET deletedat=$1 WHERE userid=$2 AND method=$3 AND done=false",
			now, userId, cred.Method)
		// Assume that the record exists and try to update it: undelete, update timestamp and response value.
		res, err := tx.Exec("UPDATE credentials SET updatedat=$1,deletedat=NULL,resp=$2,done=0 WHERE synthetic=$3",
			cred.UpdatedAt, cred.Resp, synth)
		if err != nil {
			return false, err
		}
		// If record was updated, then all is fine.
		if numrows, _ := res.RowsAffected(); numrows > 0 {
			return false, tx.Commit()
		}
	} else {
		// Hard-deleting unconformed record if it exists.
		_, err = tx.Exec("DELETE FROM credentials WHERE synthetic=$1", cred.User+":"+synth)
		if err != nil {
			return false, err
		}
	}
	// Add new record.
	_, err = tx.Exec("INSERT INTO credentials(createdat,updatedat,method,value,synthetic,userid,resp,done) "+
		"VALUES($1,$2,$3,$4,$5,$6,$7,$8)",
		cred.CreatedAt, cred.UpdatedAt, cred.Method, cred.Value, synth, userId, cred.Resp, cred.Done)
	if err != nil {
		if isDupe(err) {
			return true, t.ErrDuplicate
		}
		return true, err
	}
	return true, tx.Commit()
}

// CredIsConfirmed returns true of the given validation method is confirmed.
func (a *adapter) CredIsConfirmed(uid t.Uid, method string) (bool, error) {
	var done int
	// There could be more than one credential of the same method. We just need one.
	err := a.db.Get(&done, "SELECT done FROM credentials WHERE userid=$1 AND method=$2 AND done=true",
		store.DecodeUid(uid), method)
	if err == sql.ErrNoRows {
		// Nothing found, clear the error, otherwise it will be reported as internal error.
		err = nil
	}

	return done > 0, err
}

// credDel deletes given validation method or all methods of the given user.
// 1. If user is being deleted, hard-delete all records (method == "")
// 2. If one value is being deleted:
// 2.1 Delete it if it's valiated or if there were no attempts at validation
// (otherwise it could be used to circumvent the limit on validation attempts).
// 2.2 In that case mark it as soft-deleted.
func credDel(tx *sqlx.Tx, uid t.Uid, method, value string) error {
	constraints := " WHERE userid=$1"
	queryVar := 1
	args := []interface{}{store.DecodeUid(uid)}

	if method != "" {
		queryVar++
		constraints += " AND method=$" + strconv.Itoa(queryVar)
		args = append(args, method)

		if value != "" {
			queryVar++
			constraints += " AND value=$" + strconv.Itoa(queryVar)
			args = append(args, value)
		}
	}

	if method == "" {
		_, err := tx.Exec("DELETE FROM credentials"+constraints, args...)
		return err
	}

	// Case 2.1
	if _, err := tx.Exec("DELETE FROM credentials"+constraints+" AND (done=true OR retries=0)", args...); err != nil {
		return err
	}

	for i := 1; i <= queryVar; i++ {
		constraints = strings.Replace(constraints, "$"+strconv.Itoa(i), "$"+strconv.Itoa(i+1), 1)
	}
	// Case 2.2
	args = append([]interface{}{t.TimeNow()}, args...)
	_, err := tx.Exec("UPDATE credentials SET deletedat=$1"+constraints, args...)

	return err
}

// CredDel deletes either credentials of the given user. If method is blank all
// credentials are removed. If value is blank all credentials of the given the
// method are removed.
func (a *adapter) CredDel(uid t.Uid, method, value string) error {
	tx, err := a.db.Beginx()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	err = credDel(tx, uid, method, value)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CredConfirm marks given credential method as confirmed.
func (a *adapter) CredConfirm(uid t.Uid, method string) error {
	res, err := a.db.Exec(
		"UPDATE credentials SET updatedat=$1,done=true,synthetic=CONCAT(method,':',value) "+
			"WHERE userid=$2 AND method=$3 AND deletedat IS NULL AND done=false",
		t.TimeNow(), store.DecodeUid(uid), method)
	if err != nil {
		if isDupe(err) {
			return t.ErrDuplicate
		}
		return err
	}
	if numrows, _ := res.RowsAffected(); numrows < 1 {
		return t.ErrNotFound
	}
	return nil
}

// CredFail increments failure count of the given validation method.
func (a *adapter) CredFail(uid t.Uid, method string) error {
	_, err := a.db.Exec("UPDATE credentials SET updatedat=$1,retries=retries+1 WHERE userid=$2 AND method=$3 AND done=false",
		t.TimeNow(), store.DecodeUid(uid), method)
	return err
}

// CredGetActive returns currently active unvalidated credential of the given user and method.
func (a *adapter) CredGetActive(uid t.Uid, method string) (*t.Credential, error) {
	var cred t.Credential
	err := a.db.Get(&cred, "SELECT createdat,updatedat,method,value,resp,done,retries "+
		"FROM credentials WHERE userid=$1 AND deletedat IS NULL AND method=$2 AND done=false",
		store.DecodeUid(uid), method)
	if err != nil {
		if err == sql.ErrNoRows {
			err = nil
		}
		return nil, err
	}
	cred.User = uid.String()

	return &cred, nil
}

// CredGetAll returns credential records for the given user and method, all or validated only.
func (a *adapter) CredGetAll(uid t.Uid, method string, validatedOnly bool) ([]t.Credential, error) {
	query := "SELECT createdat,updatedat,method,value,resp,done,retries FROM credentials WHERE userid=$1 AND deletedat IS NULL"
	queryVar := 1
	args := []interface{}{store.DecodeUid(uid)}
	if method != "" {
		queryVar++
		query += " AND method=$" + strconv.Itoa(queryVar)
		args = append(args, method)
	}
	if validatedOnly {
		query += " AND done=true"
	}

	var credentials []t.Credential
	err := a.db.Select(&credentials, query, args...)
	if err != nil {
		return nil, err
	}

	user := uid.String()
	for i := range credentials {
		credentials[i].User = user
	}

	return credentials, err
}

// FileUploads

// FileStartUpload initializes a file upload
func (a *adapter) FileStartUpload(fd *t.FileDef) error {
	_, err := a.db.Exec("INSERT INTO fileuploads(id,createdat,updatedat,userid,status,mimetype,size,location)"+
		" VALUES($1,$2,$3,$4,$5,$6,$7,$8)",
		store.DecodeUid(fd.Uid()), fd.CreatedAt, fd.UpdatedAt,
		store.DecodeUid(t.ParseUid(fd.User)), fd.Status, fd.MimeType, fd.Size, fd.Location)
	return err
}

// FileFinishUpload marks file upload as completed, successfully or otherwise
func (a *adapter) FileFinishUpload(fid string, status int, size int64) (*t.FileDef, error) {
	id := t.ParseUid(fid)
	if id.IsZero() {
		return nil, t.ErrMalformed
	}

	fd, err := a.FileGet(fid)
	if err != nil {
		return nil, err
	}
	if fd == nil {
		return nil, t.ErrNotFound
	}

	fd.UpdatedAt = t.TimeNow()
	_, err = a.db.Exec("UPDATE fileuploads SET updatedat=$1,status=$2,size=$3 WHERE id=$4",
		fd.UpdatedAt, status, size, store.DecodeUid(id))
	if err == nil {
		fd.Status = status
		fd.Size = size
	} else {
		fd = nil
	}
	return fd, err
}

// FileGet fetches a record of a specific file
func (a *adapter) FileGet(fid string) (*t.FileDef, error) {
	id := t.ParseUid(fid)
	if id.IsZero() {
		return nil, t.ErrMalformed
	}

	var fd t.FileDef
	err := a.db.Get(&fd, "SELECT id,createdat,updatedat,userid AS user,status,mimetype,size,location "+
		"FROM fileuploads WHERE id=$1", store.DecodeUid(id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	fd.Id = encodeUidString(fd.Id).String()
	fd.User = encodeUidString(fd.User).String()

	return &fd, nil

}

// FileDeleteUnused deletes file upload records.
func (a *adapter) FileDeleteUnused(olderThan time.Time, limit int) ([]string, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	query := "SELECT fu.id,fu.location FROM fileuploads AS fu LEFT JOIN filemsglinks AS fml ON fml.fileid=fu.id WHERE fml.id IS NULL "
	queryVar := 0
	var args []interface{}
	if !olderThan.IsZero() {
		queryVar++
		query += "AND fu.updatedat<$" + strconv.Itoa(queryVar) + " "
		args = append(args, olderThan)
	}
	if limit > 0 {
		queryVar++
		query += "LIMIT $" + strconv.Itoa(queryVar)
		args = append(args, limit)
	}

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}

	var locations []string
	var ids []interface{}
	for rows.Next() {
		var id int
		var loc string
		if err = rows.Scan(&id, &loc); err != nil {
			break
		}
		locations = append(locations, loc)
		ids = append(ids, id)
	}
	rows.Close()

	if err != nil {
		return nil, err
	}

	if len(ids) > 0 {
		query, ids, _ = sqlx.In("DELETE FROM fileuploads WHERE id IN ($1)", ids)
		_, err = tx.Exec(query, ids...)
		if err != nil {
			return nil, err
		}
	}

	return locations, tx.Commit()
}

// Helper functions

// Check if MySQL error is a Error Code: 1062. Duplicate entry ... for key ...
func isDupe(err error) bool {
	if err == nil {
		return false
	}

	/*myerr, ok := err.(*ms.MySQLError)
	return ok && myerr.Number == 1062
	*/

	return false
}

func isMissingDb(err error) bool {
	if err == nil {
		return false
	}

	fmt.Println(err)
	return strings.Contains(err.Error(), "does not exist")
}

// Convert to JSON before storing to JSON field.
func toJSON(src interface{}) []byte {
	if src == nil {
		return []byte("null")
	}

	jval, _ := json.Marshal(src)
	return jval
}

// Deserialize JSON data from DB.
func fromJSON(src interface{}) interface{} {
	if src == nil {
		return nil
	}
	if bb, ok := src.([]byte); ok {
		var out interface{}
		json.Unmarshal(bb, &out)
		return out
	}
	return nil
}

// UIDs are stored as decoded int64 values. Take decoded string representation of int64, produce UID.
func encodeUidString(str string) t.Uid {
	unum, _ := strconv.ParseInt(str, 10, 64)
	return store.EncodeUid(unum)
}

func decodeUidString(str string) int64 {
	uid := t.ParseUid(str)
	return store.DecodeUid(uid)
}

// Convert update to a list of columns and arguments.
func updateByMap(update map[string]interface{}) (cols []string, args []interface{}) {
	index := 1
	for col, arg := range update {
		col = strings.ToLower(col)
		if col == "public" || col == "private" {
			arg = toJSON(arg)
		}
		cols = append(cols, col+"=$"+strconv.Itoa(index))
		args = append(args, arg)
		index++
	}
	return
}

// If Tags field is updated, get the tags so tags table cab be updated too.
func extractTags(update map[string]interface{}) []string {
	var tags []string

	val := update["Tags"]
	if val != nil {
		tags, _ = val.(t.StringSlice)
	}

	return []string(tags)
}

func init() {
	store.RegisterAdapter(adapterName, &adapter{})
}