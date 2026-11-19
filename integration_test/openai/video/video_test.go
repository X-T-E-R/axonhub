package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looplj/axonhub/openai_test/internal/testutil"
	"github.com/openai/openai-go/v3"
)

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}

func TestVideoCreate_Text(t *testing.T) {
	helper := testutil.NewTestHelper(t, "TestVideoCreate_SDK")
	ctx := helper.CreateTestContext()

	videoModel := strings.TrimSpace(os.Getenv("TEST_VIDEO_MODEL"))
	if videoModel == "" {
		videoModel = "sora-2"
	}

	seconds, err := getVideoSeconds()
	helper.AssertNoError(t, err, "Invalid TEST_VIDEO_SECONDS")

	size := strings.TrimSpace(os.Getenv("TEST_VIDEO_SIZE"))
	if size == "" {
		size = "1280x720"
	}

	video, err := helper.Client.Videos.New(ctx, openai.VideoNewParams{
		Prompt:  "A cat walking on the beach at sunset",
		Model:   openai.VideoModel(videoModel),
		Seconds: seconds,
		Size:    openai.VideoSize(size),
	}, helper.GetHeaderOptions()...)
	helper.AssertNoError(t, err, "Failed to create video")

	if video == nil || strings.TrimSpace(video.ID) == "" {
		t.Fatalf("Expected non-empty video id")
	}
	if strings.TrimSpace(string(video.Status)) == "" {
		t.Fatalf("Expected non-empty status")
	}
}

func TestVideoCreate_WithImage(t *testing.T) {
	helper := testutil.NewTestHelper(t, "TestVideoCreate_SDK_WithReferenceFile")
	ctx := helper.CreateTestContext()

	videoModel := strings.TrimSpace(os.Getenv("TEST_VIDEO_MODEL"))
	if videoModel == "" {
		videoModel = "sora-2"
	}

	referenceImage := strings.TrimSpace(os.Getenv("TEST_VIDEO_REFERENCE_IMAGE"))
	var refReader namedBytesReader
	if referenceImage != "" {
		refReader = newNamedBytesReader(t, referenceImage, "image/png")
	} else {
		refReader = namedBytesReader{
			Reader: bytes.NewReader([]byte{
				0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
				0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
				0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
				0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
				0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
				0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
				0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
				0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
				0x42, 0x60, 0x82,
			}),
			filename:    "ref.png",
			contentType: "image/png",
		}
	}

	seconds, err := getVideoSeconds()
	helper.AssertNoError(t, err, "Invalid TEST_VIDEO_SECONDS")

	size := strings.TrimSpace(os.Getenv("TEST_VIDEO_SIZE"))
	if size == "" {
		size = "1280x720"
	}

	video, err := helper.Client.Videos.New(ctx, openai.VideoNewParams{
		Prompt:         "A cat walking on the beach at sunset",
		InputReference: refReader,
		Model:          openai.VideoModel(videoModel),
		Seconds:        seconds,
		Size:           openai.VideoSize(size),
	}, helper.GetHeaderOptions()...)
	helper.AssertNoError(t, err, "Failed to create video with input_reference file")

	if video == nil || strings.TrimSpace(video.ID) == "" {
		t.Fatalf("Expected non-empty video id")
	}
}

func getVideoSeconds() (openai.VideoSeconds, error) {
	seconds := strings.TrimSpace(os.Getenv("TEST_VIDEO_SECONDS"))
	if seconds == "" {
		return openai.VideoSeconds4, nil
	}

	switch seconds {
	case "4":
		return openai.VideoSeconds4, nil
	case "8":
		return openai.VideoSeconds8, nil
	case "12":
		return openai.VideoSeconds12, nil
	default:
		return "4", fmt.Errorf("unsupported seconds %q (allowed: 4/8/12)", seconds)
	}
}

type namedBytesReader struct {
	*bytes.Reader
	filename    string
	contentType string
}

func newNamedBytesReader(t *testing.T, fp, contentType string) namedBytesReader {
	fileData, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("Failed to read reference image file: %v", err)
	}

	return namedBytesReader{
		Reader:      bytes.NewReader(fileData),
		filename:    filepath.Base(fp),
		contentType: contentType,
	}
}

func (b namedBytesReader) Filename() string    { return b.filename }
func (b namedBytesReader) ContentType() string { return b.contentType }
