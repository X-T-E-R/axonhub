package objects

import "time"

type RegistrationGroup struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type RegistrationInvite struct {
	ID        int        `json:"id"`
	ProjectID int        `json:"projectID"`
	GroupName string     `json:"groupName"`
	MaxUses   int        `json:"maxUses"`
	UsedUses  int        `json:"usedUses"`
	ExpiresAt *time.Time `json:"expiresAt"`
	CreatedAt time.Time  `json:"createdAt"`
	Status    string     `json:"status"`
}

type RegistrationInviteInput struct {
	ProjectID int        `json:"projectID"`
	MaxUses   int        `json:"maxUses"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type GeneratedRegistrationInvite struct {
	Code   string              `json:"code"`
	Invite *RegistrationInvite `json:"invite"`
}

// RegistrationInput deliberately has no roles, scopes, account mode or group ID.
type RegistrationInput struct {
	Email           string `json:"email"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirmPassword"`
	FirstName       string `json:"firstName"`
	LastName        string `json:"lastName"`
	InviteCode      string `json:"inviteCode"`
}
