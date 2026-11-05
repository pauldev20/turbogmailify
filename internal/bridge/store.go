package bridge

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketTokens   = []byte("tokens")   // "alias.token" -> TokenRecord
	bucketOutbound = []byte("outbound") // Gmail message id -> OutboundRecord
	bucketSends    = []byte("sends")    // big-endian unix nanos -> empty
	bucketMsgIDs   = []byte("msgids")   // stand-in Message-ID -> relayed Message-ID
	bucketMeta     = []byte("meta")
	keyHistoryID   = []byte("historyId")

	ErrTokenCollision = errors.New("token collision")
)

type TokenRecord struct {
	Alias     string
	Token     string
	Address   string
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// Outbound processing states. stateSending is written before SMTP submission
// so a crash mid-send is never retried blindly.
const (
	stateIgnored = "ignored"
	stateBlocked = "blocked"
	stateDryRun  = "dryrun"
	stateSending = "sending"
	stateSent    = "sent"
	stateFailed  = "failed"
)

type OutboundRecord struct {
	State     string
	MessageID string // Message-ID of the relayed mail
	At        time.Time
}

// Store is the bridge's persistent state in a single bbolt file.
type Store struct{ db *bolt.DB }

func openStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketTokens, bucketOutbound, bucketSends, bucketMsgIDs, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func tokenKey(alias, token string) []byte { return []byte(alias + "." + token) }

// PutToken records a correspondent, keeping FirstSeen. A different address
// under an existing token is a collision and is refused.
func (s *Store) PutToken(r TokenRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTokens)
		k := tokenKey(r.Alias, r.Token)
		if v := b.Get(k); v != nil {
			var old TokenRecord
			if err := json.Unmarshal(v, &old); err != nil {
				return err
			}
			if !strings.EqualFold(old.Address, r.Address) {
				return fmt.Errorf("%w: %s.%s", ErrTokenCollision, r.Alias, r.Token)
			}
			r.FirstSeen = old.FirstSeen
			if r.Name == "" {
				r.Name = old.Name
			}
		}
		v, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return b.Put(k, v)
	})
}

func (s *Store) Token(alias, token string) (r TokenRecord, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketTokens).Get(tokenKey(alias, token))
		if v == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(v, &r)
	})
	return
}

func (s *Store) HistoryID() (id uint64, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get(keyHistoryID); len(v) == 8 {
			id = binary.BigEndian.Uint64(v)
		}
		return nil
	})
	return
}

func (s *Store) SetHistoryID(id uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyHistoryID, binary.BigEndian.AppendUint64(nil, id))
	})
}

func (s *Store) Outbound(msgID string) (r OutboundRecord, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketOutbound).Get([]byte(msgID))
		if v == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(v, &r)
	})
	return
}

func (s *Store) SetOutbound(msgID string, r OutboundRecord) error {
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketOutbound).Put([]byte(msgID), v)
	})
}

func (s *Store) SetRelayedID(standIn, relayed string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMsgIDs).Put([]byte(standIn), []byte(relayed))
	})
}

func (s *Store) RelayedID(standIn string) (relayed string, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		relayed = string(tx.Bucket(bucketMsgIDs).Get([]byte(standIn)))
		return nil
	})
	return
}

// ReserveSend records one SMTP attempt at time at unless limit attempts were
// already made in the preceding hour. Older entries are pruned.
func (s *Store) ReserveSend(at time.Time, limit int) (ok bool, err error) {
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSends)
		cutoff := string(sendKey(at.Add(-time.Hour)))
		var old [][]byte
		n := 0
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if string(k) < cutoff {
				old = append(old, append([]byte(nil), k...))
			} else {
				n++
			}
		}
		for _, k := range old {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		if ok = n < limit; !ok {
			return nil
		}
		return b.Put(sendKey(at), []byte{})
	})
	return
}

func sendKey(t time.Time) []byte { return binary.BigEndian.AppendUint64(nil, uint64(t.UnixNano())) }
