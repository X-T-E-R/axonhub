package gql

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
)

type mysqlDialectDriver struct {
	dialect.Driver
}

func (mysqlDialectDriver) Dialect() string {
	return dialect.MySQL
}

func TestQueryChannelStats_MySQLDialectKeepsNumericChannelIDPortable(t *testing.T) {
	base := enttest.NewEntClient(t, "sqlite3", "file:analytics_mysql?mode=memory&_fk=1")
	defer base.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), base))
	project := base.Project.Create().SetName("analytics-mysql-project").SaveX(ctx)
	ch := base.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("analytics-mysql-channel").
		SetBaseURL("https://api.example.com").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SaveX(ctx)
	req := base.Request.Create().
		SetProjectID(project.ID).
		SetChannelID(ch.ID).
		SetSource(request.SourceAPI).
		SetModelID("gpt-4").
		SetFormat("openai/chat_completions").
		SetRequestBody(objects.JSONRawMessage(`{"model":"gpt-4"}`)).
		SetStatus(request.StatusCompleted).
		SetStream(false).
		SaveX(ctx)
	base.UsageLog.Create().
		SetRequestID(req.ID).
		SetProjectID(project.ID).
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetPromptTokens(11).
		SetCompletionTokens(7).
		SetTotalTokens(18).
		SetSource(usagelog.SourceAPI).
		SetFormat("openai/chat_completions").
		SaveX(ctx)

	mysqlClient := ent.NewClient(ent.Driver(mysqlDialectDriver{Driver: base.Driver()}))
	resolver := &queryResolver{Resolver: &Resolver{client: mysqlClient}}
	stats, err := resolver.queryChannelStats(ctx, nil, nil, false, time.UTC)
	require.NoError(t, err)
	require.Equal(t, []dimStats{{
		ID:           strconv.Itoa(ch.ID),
		Name:         "analytics-mysql-channel",
		RequestCount: 1,
		InputTokens:  11,
		OutputTokens: 7,
		TotalTokens:  18,
	}}, stats)
}
