package pgctl

import "github.com/jackc/pgx/v5"

// OpenSession returns the open backup session, or nil when there is none. Tests keep it
// to name a session whose timer fires late.
func (b *Backups) OpenSession() *pgx.Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn
}

// ExpireSession runs what the auto-abort timer armed for session s runs when it fires.
func (b *Backups) ExpireSession(s *pgx.Conn) { b.expire(s) }
