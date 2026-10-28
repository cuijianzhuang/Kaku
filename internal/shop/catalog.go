package shop

import (
	"context"
	"time"

	"github.com/cuijianzhuang/kaku/internal/polaris"
)

// PolarisCatalog returns what the license server can issue (plans,
// features, templates), cached for five minutes (30 seconds after a
// failure). force skips the cache, as the connection test does.
func (s *Service) PolarisCatalog(ctx context.Context, force bool) (*polaris.Catalog, error) {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	ttl := 5 * time.Minute
	if s.catErr != nil {
		ttl = 30 * time.Second
	}
	key := s.Store.Settings().PolarisURL
	if !force && key == s.catKey && !s.catAt.IsZero() && s.Now().Sub(s.catAt) < ttl {
		return s.cat, s.catErr
	}
	c := s.Polaris()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.cat, s.catErr = c.Catalog(cctx)
	s.catAt, s.catKey = s.Now(), key
	return s.cat, s.catErr
}

// PolarisCatalogOrBuiltin is the catalogue for building forms: the server's
// when it answers, otherwise the built-in list (and the reason).
func (s *Service) PolarisCatalogOrBuiltin(ctx context.Context) (*polaris.Catalog, error) {
	if !s.Polaris().Ready() {
		b := polaris.Builtin
		return &b, nil
	}
	cat, err := s.PolarisCatalog(ctx, false)
	if err != nil {
		b := polaris.Builtin
		return &b, err
	}
	return cat, nil
}
