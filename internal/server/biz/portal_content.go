package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/objects"
)

const SystemKeyPortalSettings = "system_portal_settings"

var portalSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

func ValidatePortalSettings(settings objects.PortalSettings) error {
	if len(settings.Announcements) > 100 || len(settings.Guides) > 100 {
		return fmt.Errorf("at most 100 announcements and 100 guides")
	}
	slugs := map[string]bool{}
	for _, set := range [][]*objects.PortalDocument{settings.Announcements, settings.Guides} {
		for _, doc := range set {
			if doc == nil || !portalSlug.MatchString(doc.Slug) || slugs[doc.Slug] {
				return fmt.Errorf("each document requires a unique lowercase slug")
			}
			slugs[doc.Slug] = true
			if len(doc.Title) == 0 || len(doc.Title) > 200 || len(doc.Markdown) > 128*1024 {
				return fmt.Errorf("document title or Markdown exceeds limits")
			}
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if len(data) > 1024*1024 {
		return fmt.Errorf("portal content exceeds 1 MiB")
	}
	return nil
}
func (s *SystemService) PortalSettings(ctx context.Context) (*objects.PortalSettings, error) {
	// Direct read gives publishing an immediate read-after-write contract.
	row, err := s.entFromContext(ctx).System.Query().Where(system.KeyEQ(SystemKeyPortalSettings)).Only(ctx)
	if ent.IsNotFound(err) {
		return &objects.PortalSettings{Announcements: []*objects.PortalDocument{}, Guides: []*objects.PortalDocument{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var settings objects.PortalSettings
	if err := json.Unmarshal([]byte(row.Value), &settings); err != nil {
		return nil, err
	}
	if err := ValidatePortalSettings(settings); err != nil {
		return nil, err
	}
	if settings.Announcements == nil {
		settings.Announcements = []*objects.PortalDocument{}
	}
	if settings.Guides == nil {
		settings.Guides = []*objects.PortalDocument{}
	}
	return &settings, nil
}
func (s *SystemService) SetPortalSettings(ctx context.Context, input objects.PortalSettings) (*objects.PortalSettings, error) {
	if err := ValidatePortalSettings(input); err != nil {
		return nil, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if err := s.setSystemValue(ctx, SystemKeyPortalSettings, string(data)); err != nil {
		return nil, err
	}
	return &input, nil
}
func (s *PortalService) Content(ctx context.Context) (*objects.PortalSettings, error) {
	if _, err := portalCurrentUser(ctx); err != nil {
		return nil, err
	}
	settings, err := s.system.PortalSettings(authz.WithSystemBypass(ctx, "portal-published-content"))
	if err != nil {
		return nil, err
	}
	published := func(docs []*objects.PortalDocument) []*objects.PortalDocument {
		out := []*objects.PortalDocument{}
		for _, doc := range docs {
			if doc.Published {
				out = append(out, doc)
			}
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Pinned != out[j].Pinned {
				return out[i].Pinned
			}
			return out[i].Order < out[j].Order
		})
		return out
	}
	settings.Announcements = published(settings.Announcements)
	settings.Guides = published(settings.Guides)
	return settings, nil
}

func (s *PortalService) CurrencyCode(ctx context.Context) (string, error) {
	if _, err := portalCurrentUser(ctx); err != nil {
		return "", err
	}
	settings, err := s.system.GeneralSettings(authz.WithSystemBypass(ctx, "portal-display-currency"))
	if err != nil {
		return "", err
	}
	return settings.CurrencyCode, nil
}
