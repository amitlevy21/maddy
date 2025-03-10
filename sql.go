package maddy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	specialuse "github.com/emersion/go-imap-specialuse"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-smtp"
	imapsql "github.com/foxcpp/go-imap-sql"
	"github.com/foxcpp/go-imap-sql/fsstore"
	"github.com/foxcpp/maddy/buffer"
	"github.com/foxcpp/maddy/config"
	"github.com/foxcpp/maddy/log"
	"github.com/foxcpp/maddy/module"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

type SQLStorage struct {
	back     *imapsql.Backend
	instName string
	Log      log.Logger

	storagePerDomain bool
	authPerDomain    bool
	authDomains      []string
	junkMbox         string

	resolver Resolver
}

type sqlDelivery struct {
	sqlm     *SQLStorage
	msgMeta  *module.MsgMetadata
	d        *imapsql.Delivery
	mailFrom string

	addedRcpts map[string]struct{}
}

func LookupAddr(r Resolver, ip net.IP) (string, error) {
	names, err := r.LookupAddr(context.Background(), ip.String())
	if err != nil || len(names) == 0 {
		return "", err
	}
	return strings.TrimRight(names[0], "."), nil
}

func generateReceived(r Resolver, msgMeta *module.MsgMetadata, mailFrom, rcptTo string) string {
	var received string
	if !msgMeta.DontTraceSender {
		received += "from " + msgMeta.SrcHostname
		if tcpAddr, ok := msgMeta.SrcAddr.(*net.TCPAddr); ok {
			domain, err := LookupAddr(r, tcpAddr.IP)
			if err != nil {
				received += fmt.Sprintf(" ([%v])", tcpAddr.IP)
			} else {
				received += fmt.Sprintf(" (%s [%v])", domain, tcpAddr.IP)
			}
		}
	}
	received += fmt.Sprintf(" by %s (envelope-sender <%s>)", sanitizeString(msgMeta.OurHostname), sanitizeString(mailFrom))
	received += fmt.Sprintf(" with %s id %s", msgMeta.SrcProto, msgMeta.ID)
	received += fmt.Sprintf(" for %s; %s", rcptTo, time.Now().Format(time.RFC1123Z))
	return received
}

func (sd *sqlDelivery) AddRcpt(rcptTo string) error {
	var accountName string
	// Side note: <postmaster> address will be always accepted
	// and delivered to "postmaster" account for both cases.
	if sd.sqlm.storagePerDomain {
		accountName = rcptTo
	} else {
		var err error
		accountName, _, err = splitAddress(rcptTo)
		if err != nil {
			return &smtp.SMTPError{
				Code:         501,
				EnhancedCode: smtp.EnhancedCode{5, 1, 3},
				Message:      "Invalid recipient address: " + err.Error(),
			}
		}
	}

	accountName = strings.ToLower(accountName)
	if _, ok := sd.addedRcpts[accountName]; ok {
		return nil
	}

	// This header is added to the message only for that recipient.
	// go-imap-sql does certain optimizations to store the message
	// with small amount of per-recipient data in a efficient way.
	userHeader := textproto.Header{}
	userHeader.Add("Delivered-To", rcptTo)
	userHeader.Add("Received", generateReceived(sd.sqlm.resolver, sd.msgMeta, sd.mailFrom, rcptTo))

	if err := sd.d.AddRcpt(strings.ToLower(accountName), userHeader); err != nil {
		if err == imapsql.ErrUserDoesntExists || err == backend.ErrNoSuchMailbox {
			return &smtp.SMTPError{
				Code:         550,
				EnhancedCode: smtp.EnhancedCode{5, 1, 1},
				Message:      "User doesn't exist",
			}
		}
		return err
	}

	sd.addedRcpts[accountName] = struct{}{}
	return nil
}

func (sd *sqlDelivery) Body(header textproto.Header, body buffer.Buffer) error {
	if sd.msgMeta.Quarantine.IsSet() {
		if err := sd.d.SpecialMailbox(specialuse.Junk, sd.sqlm.junkMbox); err != nil {
			return err
		}
	}

	header = header.Copy()
	header.Add("Return-Path", "<"+sanitizeString(sd.mailFrom)+">")
	return sd.d.BodyParsed(header, sd.msgMeta.BodyLength, body)
}

func (sd *sqlDelivery) Abort() error {
	return sd.d.Abort()
}

func (sd *sqlDelivery) Commit() error {
	return sd.d.Commit()
}

func (sqlm *SQLStorage) Start(msgMeta *module.MsgMetadata, mailFrom string) (module.Delivery, error) {
	d, err := sqlm.back.StartDelivery()
	if err != nil {
		return nil, err
	}
	return &sqlDelivery{
		sqlm:       sqlm,
		msgMeta:    msgMeta,
		d:          d,
		mailFrom:   mailFrom,
		addedRcpts: map[string]struct{}{},
	}, nil
}

func (sqlm *SQLStorage) Name() string {
	return "sql"
}

func (sqlm *SQLStorage) InstanceName() string {
	return sqlm.instName
}

func NewSQLStorage(_, instName string, _ []string) (module.Module, error) {
	return &SQLStorage{
		instName: instName,
		Log:      log.Logger{Name: "sql"},
		resolver: net.DefaultResolver,
	}, nil
}

func (sqlm *SQLStorage) Init(cfg *config.Map) error {
	var driver, dsn string
	var fsstoreLocation string
	appendlimitVal := int64(-1)

	opts := imapsql.Opts{}
	cfg.String("driver", false, true, "", &driver)
	cfg.String("dsn", false, true, "", &dsn)
	cfg.Int64("appendlimit", false, false, 32*1024*1024, &appendlimitVal)
	cfg.Bool("debug", true, &sqlm.Log.Debug)
	cfg.Bool("storage_perdomain", true, &sqlm.storagePerDomain)
	cfg.Bool("auth_perdomain", true, &sqlm.authPerDomain)
	cfg.StringList("auth_domains", true, false, nil, &sqlm.authDomains)
	cfg.Int("sqlite3_cache_size", false, false, 0, &opts.CacheSize)
	cfg.Int("sqlite3_busy_timeout", false, false, 0, &opts.BusyTimeout)
	cfg.Bool("sqlite3_exclusive_lock", false, &opts.ExclusiveLock)
	cfg.String("junk_mailbox", false, false, "Junk", &sqlm.junkMbox)

	cfg.Custom("fsstore", false, false, func() (interface{}, error) {
		return "", nil
	}, func(m *config.Map, node *config.Node) (interface{}, error) {
		switch len(node.Args) {
		case 0:
			if sqlm.instName == "" {
				return nil, errors.New("sql: need explicit fsstore location for inline definition")
			}
			return filepath.Join(StateDirectory(cfg.Globals), "sql-"+sqlm.instName+"-fsstore"), nil
		case 1:
			return node.Args[0], nil
		default:
			return nil, m.MatchErr("expected 0 or 1 arguments")
		}
	}, &fsstoreLocation)

	if _, err := cfg.Process(); err != nil {
		return err
	}

	if sqlm.authPerDomain && sqlm.authDomains == nil {
		return errors.New("sql: auth_domains must be set if auth_perdomain is used")
	}

	if fsstoreLocation != "" {
		if !filepath.IsAbs(fsstoreLocation) {
			fsstoreLocation = filepath.Join(StateDirectory(cfg.Globals), fsstoreLocation)
		}

		if err := os.MkdirAll(fsstoreLocation, os.ModeDir|os.ModePerm); err != nil {
			return err
		}
		opts.ExternalStore = &fsstore.Store{Root: fsstoreLocation}
	}

	if appendlimitVal == -1 {
		opts.MaxMsgBytes = nil
	} else {
		opts.MaxMsgBytes = new(uint32)
		*opts.MaxMsgBytes = uint32(appendlimitVal)
	}
	var err error
	sqlm.back, err = imapsql.New(driver, dsn, opts)
	if err != nil {
		return fmt.Errorf("sql: %s", err)
	}

	sqlm.Log.Debugln("go-imap-sql version", imapsql.VersionStr)

	return nil
}

func (sqlm *SQLStorage) IMAPExtensions() []string {
	return []string{"APPENDLIMIT", "MOVE", "CHILDREN"}
}

func (sqlm *SQLStorage) Updates() <-chan backend.Update {
	return sqlm.back.Updates()
}

func (sqlm *SQLStorage) EnableChildrenExt() bool {
	return sqlm.back.EnableChildrenExt()
}

func (sqlm *SQLStorage) CheckPlain(username, password string) bool {
	accountName, ok := checkDomainAuth(username, sqlm.authPerDomain, sqlm.authDomains)
	if !ok {
		return false
	}

	return sqlm.back.CheckPlain(accountName, password)
}

func (sqlm *SQLStorage) GetOrCreateUser(username string) (backend.User, error) {
	var accountName string
	if sqlm.storagePerDomain {
		if !strings.Contains(username, "@") {
			return nil, errors.New("GetOrCreateUser: username@domain required")
		}
		accountName = username
	} else {
		parts := strings.Split(username, "@")
		accountName = parts[0]
	}

	return sqlm.back.GetOrCreateUser(accountName)
}

func init() {
	module.Register("sql", NewSQLStorage)
}
