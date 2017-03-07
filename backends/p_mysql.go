package backends

import (
	"database/sql"
	"strings"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
	"github.com/go-sql-driver/mysql"

	"github.com/flashmob/go-guerrilla/response"
	"runtime/debug"
)

// ----------------------------------------------------------------------------------
// Processor Name: mysql
// ----------------------------------------------------------------------------------
// Description   : Saves the e.Data (email data) and e.DeliveryHeader together in mysql
//               : using the hash generated by the "hash" processor and stored in
//               : e.Hashes
// ----------------------------------------------------------------------------------
// Config Options: mail_table string - mysql table name
//               : mysql_db string - mysql database name
//               : mysql_host string - mysql host name, eg. 127.0.0.1
//               : mysql_pass string - mysql password
//               : mysql_user string - mysql username
//               : primary_mail_host string - primary host name
// --------------:-------------------------------------------------------------------
// Input         : e.Data
//               : e.DeliveryHeader generated by ParseHeader() processor
//               : e.MailFrom
//               : e.Subject - generated by by ParseHeader() processor
// ----------------------------------------------------------------------------------
// Output        : Sets e.QueuedId with the first item fromHashes[0]
// ----------------------------------------------------------------------------------
func init() {
	processors["mysql"] = func() Decorator {
		return MySql()
	}
}

const procMySQLReadTimeout = time.Second * 10
const procMySQLWriteTimeout = time.Second * 10

type MysqlProcessorConfig struct {
	MysqlTable  string `json:"mail_table"`
	MysqlDB     string `json:"mysql_db"`
	MysqlHost   string `json:"mysql_host"`
	MysqlPass   string `json:"mysql_pass"`
	MysqlUser   string `json:"mysql_user"`
	PrimaryHost string `json:"primary_mail_host"`
}

type MysqlProcessor struct {
	cache  stmtCache
	config *MysqlProcessorConfig
}

func (m *MysqlProcessor) connect(config *MysqlProcessorConfig) (*sql.DB, error) {
	var db *sql.DB
	var err error
	conf := mysql.Config{
		User:         config.MysqlUser,
		Passwd:       config.MysqlPass,
		DBName:       config.MysqlDB,
		Net:          "tcp",
		Addr:         config.MysqlHost,
		ReadTimeout:  procMySQLReadTimeout,
		WriteTimeout: procMySQLWriteTimeout,
		Params:       map[string]string{"collation": "utf8_general_ci"},
	}
	if db, err = sql.Open("mysql", conf.FormatDSN()); err != nil {
		Log().Error("cannot open mysql", err)
		return nil, err
	}
	// do we have permission to access the table?
	_, err = db.Query("SELECT mail_id FROM " + m.config.MysqlTable + "LIMIT 1")
	if err != nil {
		Log().Error("cannot select table", err)
		return nil, err
	}
	Log().Info("connected to mysql on tcp ", config.MysqlHost)
	return db, err
}

// prepares the sql query with the number of rows that can be batched with it
func (g *MysqlProcessor) prepareInsertQuery(rows int, db *sql.DB) *sql.Stmt {
	if rows == 0 {
		panic("rows argument cannot be 0")
	}
	if g.cache[rows-1] != nil {
		return g.cache[rows-1]
	}
	sqlstr := "INSERT INTO " + g.config.MysqlTable + " "
	sqlstr += "(`date`, `to`, `from`, `subject`, `body`, `charset`, `mail`, `spam_score`, `hash`, `content_type`, `recipient`, `has_attach`, `ip_addr`, `return_path`, `is_tls`)"
	sqlstr += " values "
	values := "(NOW(), ?, ?, ?, ? , 'UTF-8' , ?, 0, ?, '', ?, 0, ?, ?, ?)"
	// add more rows
	comma := ""
	for i := 0; i < rows; i++ {
		sqlstr += comma + values
		if comma == "" {
			comma = ","
		}
	}
	stmt, sqlErr := db.Prepare(sqlstr)
	if sqlErr != nil {
		Log().WithError(sqlErr).Panic("failed while db.Prepare(INSERT...)")
	}
	// cache it
	g.cache[rows-1] = stmt
	return stmt
}

func (g *MysqlProcessor) doQuery(c int, db *sql.DB, insertStmt *sql.Stmt, vals *[]interface{}) {
	var execErr error
	defer func() {
		if r := recover(); r != nil {
			Log().Error("Recovered form panic:", r, string(debug.Stack()))
			sum := 0
			for _, v := range *vals {
				if str, ok := v.(string); ok {
					sum = sum + len(str)
				}
			}
			Log().Errorf("panic while inserting query [%s] size:%d, err %v", r, sum, execErr)
			panic("query failed")
		}
	}()
	// prepare the query used to insert when rows reaches batchMax
	insertStmt = g.prepareInsertQuery(c, db)
	_, execErr = insertStmt.Exec(*vals...)
	if execErr != nil {
		Log().WithError(execErr).Error("There was a problem the insert")
	}
}

func MySql() Decorator {

	var config *MysqlProcessorConfig
	var vals []interface{}
	var db *sql.DB
	mp := &MysqlProcessor{}

	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		configType := BaseConfig(&MysqlProcessorConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}
		config = bcfg.(*MysqlProcessorConfig)
		mp.config = config
		db, err = mp.connect(config)
		if err != nil {
			Log().Errorf("cannot open mysql: %s", err)
			return err
		}
		return nil
	}))

	// shutdown
	Svc.AddShutdowner(ShutdownWith(func() error {
		if db != nil {
			return db.Close()
		}
		return nil
	}))

	return func(c Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {

			if task == TaskSaveMail {
				var to, body string
				to = trimToLimit(strings.TrimSpace(e.RcptTo[0].User)+"@"+config.PrimaryHost, 255)
				hash := ""
				if len(e.Hashes) > 0 {
					hash = e.Hashes[0]
					e.QueuedId = e.Hashes[0]
				}

				var co *compressor
				// a compressor was set by the Compress processor
				if c, ok := e.Values["zlib-compressor"]; ok {
					body = "gzip"
					co = c.(*compressor)
				}
				// was saved in redis by the Redis processor
				if _, ok := e.Values["redis"]; ok {
					body = "redis"
				}

				// build the values for the query
				vals = []interface{}{} // clear the vals
				vals = append(vals,
					to,
					trimToLimit(e.MailFrom.String(), 255),
					trimToLimit(e.Subject, 255),
					body)
				if body == "redis" {
					// data already saved in redis
					vals = append(vals, "")
				} else if co != nil {
					// use a compressor (automatically adds e.DeliveryHeader)
					vals = append(vals, co.String())

				} else {
					vals = append(vals, e.String())
				}

				vals = append(vals,
					hash,
					to,
					e.RemoteIP,
					trimToLimit(e.MailFrom.String(), 255),
					e.TLS)

				stmt := mp.prepareInsertQuery(1, db)
				mp.doQuery(1, db, stmt, &vals)
				// continue to the next Processor in the decorator chain
				return c.Process(e, task)
			} else if task == TaskValidateRcpt {
				// if you need to validate the e.Rcpt then change to:
				if len(e.RcptTo) > 0 {
					// since this is called each time a recipient is added
					// validate only the _last_ recipient that was appended
					last := e.RcptTo[len(e.RcptTo)-1]
					if len(last.User) > 255 {
						// TODO what kind of response to send?
						return NewResult(response.Canned.FailNoSenderDataCmd), NoSuchUser
					}
				}
				return c.Process(e, task)
			} else {
				return c.Process(e, task)
			}

		})
	}
}