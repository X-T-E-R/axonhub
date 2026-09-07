package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/tracing"
)

func TestForwardingObservationThreadTraceBarrier(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:observation-middleware?mode=memory&_fk=1")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	project := client.Project.Create().SetName("isolated-observation").SaveX(ctx)
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{Client: client, SystemService: system})
	usage := biz.NewUsageLogService(client, system, biz.NewChannelServiceForTest(client))
	w := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{AttemptTimeout: 3 * time.Second})
	requests := biz.NewRequestServiceWithObservationWriter(client, system, usage, storage, biz.NewLiveStreamRegistry(), nil, w)
	traceSvc := biz.NewTraceService(biz.TraceServiceParams{Ent: client, RequestService: requests})
	threadSvc := biz.NewThreadService(client, traceSvc)
	require.NoError(t, w.Start(ctx))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client.Thread.Use(func(next ent.Mutator) ent.Mutator {
		return hook.ThreadFunc(func(ctx context.Context, m *ent.ThreadMutation) (ent.Value, error) {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return next.Mutate(ctx, m)
		})
	})
	router := gin.New()
	router.Use(WithThread(tracing.Config{}, threadSvc), WithTrace(tracing.Config{}, traceSvc))
	router.GET("/", func(c *gin.Context) {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Error("thread persistence did not enter barrier")
		}
		trace, ok := contexts.GetTrace(c.Request.Context())
		require.True(t, ok)
		require.Equal(t, "isolated-trace", trace.TraceID)
		c.Data(http.StatusOK, "text/event-stream", []byte("data: [DONE]\n\n"))
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(contexts.WithProjectID(ctx, project.ID))
	req.Header.Set("Ah-Thread-Id", "isolated-thread")
	req.Header.Set("Ah-Trace-Id", "isolated-trace")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "data: [DONE]\n\n", string(body))
	close(release)
	drain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	thread := client.Thread.Query().OnlyX(ctx)
	trace := client.Trace.Query().OnlyX(ctx)
	require.Equal(t, thread.ID, trace.ThreadID)
	require.NoError(t, w.Stop(drain))
}
