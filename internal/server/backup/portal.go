package backup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func (svc *BackupService) backupPortalState(ctx context.Context, o *objWriter, opts BackupOptions) error {
	if opts.IncludeAPIKeys {
		keys, err := svc.db.APIKey.Query().Where(apikey.PortalManagedEQ(true)).WithUser().All(ctx)
		if err != nil {
			return err
		}
		owners := []*BackupPortalOwner{}
		seen := map[int]bool{}
		for _, key := range keys {
			u := key.Edges.User
			if u == nil {
				return fmt.Errorf("managed key owner unavailable")
			}
			if seen[u.ID] {
				continue
			}
			seen[u.ID] = true
			owners = append(owners, &BackupPortalOwner{Email: u.Email, AccessMode: string(u.AccessMode)})
		}
		data, err := json.Marshal(owners)
		if err != nil {
			return err
		}
		if err := o.rawField("portal_owners", data); err != nil {
			return err
		}
	}
	if opts.IncludeProjects {
		row, err := svc.db.System.Query().Where(system.KeyEQ(biz.SystemKeyPortalSettings)).Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var content objects.PortalSettings
		if err := json.Unmarshal([]byte(row.Value), &content); err != nil {
			return err
		}
		if err := biz.ValidatePortalSettings(content); err != nil {
			return err
		}
		data, err := json.Marshal(content)
		if err != nil {
			return err
		}
		return o.rawField("portal_settings", data)
	}
	return nil
}

func restorePortalState(ctx context.Context, db *ent.Client, data BackupData, opts RestoreOptions) error {
	if opts.IncludeAPIKeys {
		for _, owner := range data.PortalOwners {
			if owner == nil {
				return fmt.Errorf("invalid managed owner reference")
			}
			target, err := db.User.Query().Where(user.EmailEQ(owner.Email)).Only(ctx)
			if err != nil || target.IsOwner || string(target.AccessMode) != owner.AccessMode {
				return fmt.Errorf("provision managed owner identity and access mode before restore")
			}
		}
	}
	if opts.IncludeProjects && data.PortalSettings != nil {
		if err := biz.ValidatePortalSettings(*data.PortalSettings); err != nil {
			return err
		}
		raw, err := json.Marshal(data.PortalSettings)
		if err != nil {
			return err
		}
		row, err := db.System.Query().Where(system.KeyEQ(biz.SystemKeyPortalSettings)).Only(ctx)
		if ent.IsNotFound(err) {
			_, err = db.System.Create().SetKey(biz.SystemKeyPortalSettings).SetValue(string(raw)).Save(ctx)
		} else if err == nil {
			_, err = db.System.UpdateOne(row).SetValue(string(raw)).Save(ctx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func resolvePortalOwner(ctx context.Context, db *ent.Client, data *BackupAPIKey) (int, error) {
	if data.Type != apikey.TypePersonal || data.OwnerEmail == "" || data.ProjectName == "" {
		return 0, fmt.Errorf("managed key requires personal type and stable owner/group references")
	}
	owner, err := db.User.Query().Where(user.EmailEQ(data.OwnerEmail), user.StatusEQ(user.StatusActivated), user.IsOwnerEQ(false)).Only(ctx)
	if err != nil {
		return 0, fmt.Errorf("managed key owner identity unavailable")
	}
	return owner.ID, nil
}

// Portal references require a source identity from the channel manifest and a
// name-resolved destination. A coincidentally equal destination ID is not proof.
func portalResolvedChannelIDs(channels []*BackupChannel, idMap map[int]int) map[int]int {
	byID := map[int]string{}
	byName := map[string]int{}
	ambiguous := map[int]bool{}
	for _, ch := range channels {
		if ch == nil || ch.ID == 0 || ch.Name == "" {
			continue
		}
		if name, ok := byID[ch.ID]; ok && name != ch.Name {
			ambiguous[ch.ID] = true
		}
		if id, ok := byName[ch.Name]; ok && id != ch.ID {
			ambiguous[id], ambiguous[ch.ID] = true, true
		}
		byID[ch.ID], byName[ch.Name] = ch.Name, ch.ID
	}
	resolved := map[int]int{}
	for id := range byID {
		if target, ok := idMap[id]; ok && !ambiguous[id] {
			resolved[id] = target
		}
	}
	return resolved
}

func requirePortalChannelIDs(ids []int, resolved map[int]int) error {
	for _, id := range ids {
		if _, ok := resolved[id]; !ok {
			return fmt.Errorf("portal channel reference %d cannot be resolved by source name", id)
		}
	}
	return nil
}

func validatePortalProjectChannels(ctx context.Context, db *ent.Client, data *BackupProject, backup BackupData, opts RestoreOptions, resolved map[int]int) error {
	if data.Profiles == nil {
		return nil // Existing destination profiles are kept, not source references.
	}
	existing, err := db.Project.Query().Where(project.NameEQ(data.Name)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}
	if existing != nil && opts.ProjectConflictStrategy != ConflictStrategyOverwrite {
		return nil
	}
	portal := false
	for _, p := range data.Profiles.Profiles {
		portal = portal || p.ModelIDs != nil
	}
	// Legacy backups omit ModelIDs; restoreProjects preserves the destination's
	// explicit model limit, so its new channel references also need validation.
	if existing != nil && existing.Profiles != nil {
		for _, p := range existing.Profiles.Profiles {
			portal = portal || p.ModelIDs != nil
		}
	}
	if opts.IncludeAPIKeys {
		for _, key := range backup.APIKeys {
			portal = portal || (key != nil && key.PortalManaged && key.ProjectName == data.Name)
		}
	}
	if !portal && existing != nil {
		portal, err = db.APIKey.Query().Where(apikey.ProjectIDEQ(existing.ID), apikey.PortalManagedEQ(true)).Exist(ctx)
		if err != nil {
			return err
		}
	}
	if portal {
		for _, p := range data.Profiles.Profiles {
			if err := requirePortalChannelIDs(p.ChannelIDs, resolved); err != nil {
				return fmt.Errorf("restore portal group %q profile %q: %w", data.Name, p.Name, err)
			}
		}
	}
	return nil
}
