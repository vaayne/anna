package mcp

import (
	"context"
	"strings"
)

// Catalog sources are prefixed into IDs and page tokens so a second source can
// be added without colliding: ID "official", page token "official:<cursor>".
const (
	RegistrySourceOfficial = "official"

	// maxUpstreamPagesPerRequest bounds one search: the registry has no
	// transport filter, so a sparse query can burn pages without matches.
	maxUpstreamPagesPerRequest = 5

	registryAuthNone        = "none"
	registryAuthBearer      = "bearer"
	registryAuthUnsupported = "unsupported"
)

// CatalogHeader is one install-time header requirement advertised by the
// registry. Template is the raw value template (e.g. "Bearer {smithery_api_key}")
// so the UI can label what the user must supply; it is never a secret itself.
type CatalogHeader struct {
	Name        string
	Template    string
	Required    bool
	Secret      bool
	Description string
}

// CatalogServer is one marketplace row. ID is the registry identifier
// (e.g. "com.notion/mcp"); the connection URL comes from the first
// streamable-http remote.
type CatalogServer struct {
	Source      string
	ID          string
	Name        string
	Description string
	Version     string
	URL         string
	Transport   string
	Auth        string // none | bearer | unsupported
	Headers     []CatalogHeader
	Repository  string
}

// CatalogPage is one page of catalog results with an opaque next token.
type CatalogPage struct {
	Servers       []CatalogServer
	NextPageToken string
}

// Catalog is the marketplace read surface; the MCP registry API and the Web UI
// consume it. Writes (install) go through the ordinary registration create.
type Catalog interface {
	Search(ctx context.Context, q string, pageSize int, pageToken string) (CatalogPage, error)
	Get(ctx context.Context, source, id string) (CatalogServer, error)
}

// inferAuth applies the registry auth classification: no headers → none;
// exactly one Authorization header whose template starts with "Bearer " →
// bearer; anything else required → unsupported (needs manual setup).
func inferAuth(headers []CatalogHeader) string {
	var authHeader *CatalogHeader
	for i := range headers {
		h := &headers[i]
		switch {
		case !strings.EqualFold(h.Name, "Authorization"):
			// Optional custom headers are surfaced but do not force the label.
			if h.Required {
				return registryAuthUnsupported
			}
		case authHeader != nil:
			return registryAuthUnsupported
		default:
			authHeader = h
		}
	}
	switch {
	case authHeader == nil:
		return registryAuthNone
	case strings.HasPrefix(authHeader.Template, "Bearer "):
		return registryAuthBearer
	default:
		return registryAuthUnsupported
	}
}

// dedupeServers keeps the first occurrence per (id, version) pair — the
// registry can return the same server several times per page — preserving the
// upstream order.
func dedupeServers(in []CatalogServer) []CatalogServer {
	seen := make(map[string]struct{}, len(in))
	out := make([]CatalogServer, 0, len(in))
	for _, s := range in {
		key := s.Source + "|" + s.ID + "|" + s.Version
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}
