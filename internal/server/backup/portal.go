package backup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
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
