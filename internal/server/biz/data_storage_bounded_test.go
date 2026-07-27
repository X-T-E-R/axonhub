package biz

import (
	"bytes"
	"context"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/objects"
)

type boundedObjectStoreFake struct {
	payload   []byte
	size      int64
	readBytes int
	openCalls int
	getCalls  int
}

func (f *boundedObjectStoreFake) PutObject(context.Context, string, []byte) error {
	return nil
}
func (f *boundedObjectStoreFake) PutObjectStream(context.Context, string, io.Reader, int64) (int64, error) {
	return 0, nil
}
func (f *boundedObjectStoreFake) GetObject(context.Context, string) ([]byte, error) {
	f.getCalls++
	return append([]byte(nil), f.payload...), nil
}
func (f *boundedObjectStoreFake) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	f.openCalls++
	reader := &boundedTestCountingReader{reader: bytes.NewReader(f.payload), count: &f.readBytes}
	return io.NopCloser(reader), f.size, nil
}
func (f *boundedObjectStoreFake) DeleteObject(context.Context, string) error { return nil }

type boundedTestCountingReader struct {
	reader io.Reader
	count  *int
}

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	copy(p, "data")
	r.cancel()
	return 4, nil
}

func (r *boundedTestCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	*r.count += n
	return n, err
}

func TestDataStorageServiceLoadDataBoundedObjectStore(t *testing.T) {
	ds := &ent.DataStorage{
		ID:       41,
		Type:     datastorage.TypeS3,
		Settings: &objects.DataStorageSettings{S3: &objects.S3{}},
	}

	t.Run("known oversize rejected before read", func(t *testing.T) {
		store := &boundedObjectStoreFake{payload: []byte("123456"), size: 6}
		service := &DataStorageService{objectStoreCache: map[int]ObjectStore{ds.ID: store}}
		_, err := service.LoadDataBounded(context.Background(), ds, "evidence.json", 5)
		require.ErrorIs(t, err, ErrDataTooLarge)
		require.Equal(t, 1, store.openCalls)
		require.Zero(t, store.getCalls)
		require.Zero(t, store.readBytes)
	})

	t.Run("unknown size reads only max plus one", func(t *testing.T) {
		store := &boundedObjectStoreFake{payload: []byte("123456789"), size: -1}
		service := &DataStorageService{objectStoreCache: map[int]ObjectStore{ds.ID: store}}
		_, err := service.LoadDataBounded(context.Background(), ds, "evidence.json", 5)
		require.ErrorIs(t, err, ErrDataTooLarge)
		require.Equal(t, 6, store.readBytes)
		require.Zero(t, store.getCalls)
	})

	t.Run("exact maximum succeeds", func(t *testing.T) {
		store := &boundedObjectStoreFake{payload: []byte("12345"), size: -1}
		service := &DataStorageService{objectStoreCache: map[int]ObjectStore{ds.ID: store}}
		data, err := service.LoadDataBounded(context.Background(), ds, "evidence.json", 5)
		require.NoError(t, err)
		require.Equal(t, []byte("12345"), data)
		require.Equal(t, 5, store.readBytes)
	})
}

func TestReadAllBoundedStopsWhenContextIsCanceledDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := readAllBounded(ctx, &cancelAfterFirstRead{cancel: cancel}, -1, 1024)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLoadDataBoundedRejectsNonCancelableBackends(t *testing.T) {
	service := &DataStorageService{}
	for _, backend := range []datastorage.Type{datastorage.TypeGcs, datastorage.TypeWebdav} {
		t.Run(backend.String(), func(t *testing.T) {
			_, err := service.LoadDataBounded(context.Background(), &ent.DataStorage{Type: backend}, "evidence.json", 1024)
			require.ErrorIs(t, err, ErrBoundedReadUnsupported)
			var unsupported *BoundedReadUnsupportedError
			require.ErrorAs(t, err, &unsupported)
			require.Equal(t, backend, unsupported.Backend)
		})
	}
}

func TestReadAllBoundedMaxInt64DoesNotOverflow(t *testing.T) {
	data, err := readAllBounded(context.Background(), bytes.NewBufferString("safe"), -1, math.MaxInt64)
	require.NoError(t, err)
	require.Equal(t, []byte("safe"), data)
}
