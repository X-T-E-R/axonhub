package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
)

func TestRegistrationHTTPReviewRegressions(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:registration-regressions-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: cache})
	db.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("synthetic-regression-jwt").SaveX(ctx)
	users := biz.NewUserService(biz.UserServiceParams{Ent: db, CacheConfig: cache})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: db, SystemService: system, UserService: users})
	owner := db.User.Create().SetEmail("owner@example.test").SetPassword("unused").SetIsOwner(true).SaveX(ctx)
	ids := []string{"m"}
	group := db.Project.Create().SetName("Regression group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: []int{1}}}}).SaveX(ctx)
	handlers := NewAuthHandlers(AuthHandlersParams{AuthService: auth})
	graph := gql.NewGraphqlHandlers(gql.Dependencies{Ent: db, AuthService: auth, UserService: users, SystemService: system})
	router := gin.New()
	router.Use(middleware.WithEntClient(db))
	router.POST("/admin/auth/signup", handlers.SignUp)
	router.POST("/admin/auth/signin", handlers.SignIn)
	router.POST("/admin/graphql", middleware.WithJWTAuth(auth), middleware.WithLegacyUser(), middleware.WithProjectID(), func(c *gin.Context) { graph.Graphql.ServeHTTP(c.Writer, c.Request) })
	request := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Project-ID", fmt.Sprintf("gid://axonhub/Project/%d", group.ID))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	graphRequest := func(t *testing.T, token, query string) json.RawMessage {
		t.Helper()
		w := request("/admin/graphql", token, map[string]any{"query": query})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			Data   json.RawMessage
			Errors json.RawMessage
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Empty(t, body.Errors, w.Body.String())
		return body.Data
	}
	ownerToken, err := auth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)
	graphRequest(t, ownerToken, "mutation{setInvitationRegistration(enabled:true)}")
	createQuery := fmt.Sprintf("mutation{createRegistrationInvite(input:{projectID:%d,maxUses:3}){code invite{id}}}", group.ID)
	t.Run("mixed_case_relogin", func(t *testing.T) {
		raw := graphRequest(t, ownerToken, createQuery)
		var generated struct {
			CreateRegistrationInvite struct {
				Code   string
				Invite struct{ ID int }
			}
		}
		require.NoError(t, json.Unmarshal(raw, &generated))
		invite := generated.CreateRegistrationInvite
		input := objects.RegistrationInput{Email: "New.Case@Example.test", Password: "SyntheticPassword1", ConfirmPassword: "SyntheticPassword1", FirstName: "First", LastName: "Last", InviteCode: invite.Code}
		w := request("/admin/auth/signup", "", input)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		u := db.User.Query().Where(user.EmailEQ(strings.ToLower(input.Email))).OnlyX(ctx)
		for _, email := range []string{u.Email, input.Email} {
			// Each request is anonymous: the second does not reuse the first JWT.
			w = request("/admin/auth/signin", "", SignInRequest{Email: email, Password: input.Password})
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var signed SignInResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &signed))
			got, err := auth.AuthenticateJWTToken(t.Context(), signed.Token)
			require.NoError(t, err)
			require.Equal(t, u.ID, got.ID)
		}
		input.Email = "NEW.CASE@example.test"
		w = request("/admin/auth/signup", "", input)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), biz.ErrRegistrationEmail.Error())
		require.Equal(t, 1, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).UsedUses)
		t.Log("F-05 HTTP: mixed-case signup=201, first signin=200, original-spelling anonymous relogin=200, normalized duplicate=400 with unchanged consumption")
	})
	t.Run("system_scope_consumers", func(t *testing.T) {
		for _, kind := range []string{"direct wildcard", "system role wildcard", "explicit scopes"} {
			t.Run(kind, func(t *testing.T) {
				grants := []string{"*"}
				if kind == "explicit scopes" {
					grants = []string{"write_settings", "write_users"}
				}
				u, err := users.CreateUser(ctx, ent.CreateUserInput{Email: uuid.NewString() + "@example.test", Password: "SyntheticPassword1", Scopes: grants})
				require.NoError(t, err)
				if kind == "system role wildcard" {
					r := db.Role.Create().SetName("Registration wildcard").SetScopes(grants).SaveX(ctx)
					db.User.UpdateOne(u).SetScopes([]string{}).AddRoleIDs(r.ID).SaveX(ctx)
				}
				token, err := auth.GenerateJWTToken(ctx, u)
				require.NoError(t, err)
				graphRequest(t, token, "mutation{setInvitationRegistration(enabled:true)}")
				graphRequest(t, token, "{registrationGroups{id name}}")
				raw := graphRequest(t, token, createQuery)
				var generated struct {
					CreateRegistrationInvite struct{ Invite struct{ ID int } }
				}
				require.NoError(t, json.Unmarshal(raw, &generated))
				id := generated.CreateRegistrationInvite.Invite.ID
				require.Positive(t, id)
				graphRequest(t, token, "{registrationInvites{id status}}")
				raw = graphRequest(t, token, fmt.Sprintf("mutation{revokeRegistrationInvite(id:%d){id status}}", id))
				require.Contains(t, string(raw), `"status":"revoked"`)
				require.True(t, db.RegistrationInvite.GetX(ctx, id).Revoked)
			})
		}
		t.Log("F-06 HTTP/JWT: direct *, system role * and explicit scopes authorize switch, groups, create, list and revoke with X-Project-ID present")
	})
}
