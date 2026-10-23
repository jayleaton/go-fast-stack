package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

const imgID = "0123456789abcdef0123456789abcdef"

var stored = services.Image{ID: imgID, ContentType: "image/webp", Width: 640, Height: 480, Size: 1234}

func newImageRouter(test *testing.T) (*mocks.MockImageService, http.Handler) {
	svc := mocks.NewMockImageService(gomock.NewController(test))
	mux := http.NewServeMux()
	api.NewImages(svc, apitest.Gate()).Routes(mux)
	return svc, mux
}

type imageRoute struct {
	method, path, body string
	invalid            string // a body the validator rejects; "" when the service decides
	expect             func(recorder *mocks.MockImageServiceMockRecorder, err error)
	file               bool // success streams the file instead of the envelope
}

var imageRoutes = map[string]imageRoute{
	"POST images (upload)": {
		method: "POST", path: r.Images, body: "\x89PNG\r\n\x1a\nrest",
		expect: func(recorder *mocks.MockImageServiceMockRecorder, err error) {
			recorder.Upload(anyCtx, actor, []byte("\x89PNG\r\n\x1a\nrest")).Return(stored, err)
		},
	},
	"POST images/import": {
		method: "POST", path: r.ImageImport, body: `{"url":" https://example.com/a.png "}`, invalid: `{"url":"javascript:alert(1)"}`,
		expect: func(recorder *mocks.MockImageServiceMockRecorder, err error) {
			recorder.Import(anyCtx, actor, validate.CreateImageFromURL{URL: "https://example.com/a.png"}).Return(stored, err)
		},
	},
	"GET image": {
		method: "GET", path: config.ImagePath(imgID), file: true,
		expect: func(recorder *mocks.MockImageServiceMockRecorder, err error) {
			file := services.ImageFile{}
			if err == nil {
				file = services.ImageFile{ContentType: "image/webp", Size: 4, Body: io.NopCloser(strings.NewReader("RIFF"))}
			}
			recorder.Open(anyCtx, actor, imgID).Return(file, err)
		},
	},
	"DELETE image": {
		method: "DELETE", path: config.ImagePath(imgID),
		expect: func(recorder *mocks.MockImageServiceMockRecorder, err error) {
			recorder.Delete(anyCtx, actor, imgID).Return(err)
		},
	},
}

// TestImageRoutes runs the four boundary cases against every endpoint.
// Images are keyed per user, so the service answers another user's id as
// not found: "other user" is a 404 here, never the image.
func TestImageRoutes(test *testing.T) {
	test.Parallel()
	for name, rt := range imageRoutes {
		test.Run(name+"/no session is 401", func(test *testing.T) {
			test.Parallel()
			_, handler := newImageRouter(test)
			wantError(test, apitest.Do(test, handler, apitest.Anonymous, rt.method, rt.path, rt.body), 401, "Unauthorized")
		})
		test.Run(name+"/other user is 404", func(test *testing.T) {
			test.Parallel()
			svc, handler := newImageRouter(test)
			rt.expect(svc.EXPECT(), services.ErrImageNotFound)
			wantError(test, apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, rt.body), 404, "Image not found")
		})
		test.Run(name+"/invalid input is 400", func(test *testing.T) {
			test.Parallel()
			svc, handler := newImageRouter(test)
			body := rt.invalid
			if body == "" {
				body = rt.body
				rt.expect(svc.EXPECT(), services.ErrImageUnsupported)
			}
			if res := apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, body); res.Status != 400 {
				test.Errorf("status %d, want 400 (%s)", res.Status, *res.Error)
			}
		})
		test.Run(name+"/ok is 200", func(test *testing.T) {
			test.Parallel()
			svc, handler := newImageRouter(test)
			rt.expect(svc.EXPECT(), nil)
			if rt.file {
				wantFile(test, handler, rt.method, rt.path)
				return
			}
			res := apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, rt.body)
			if res.Status != 200 {
				test.Fatalf("status %d (%s)", res.Status, *res.Error)
			}
			if rt.method == "POST" && apitest.Data[services.Image](test, res) != stored {
				test.Errorf("data = %s", res.Data)
			}
		})
	}
}

// wantFile: GET streams the stored bytes with their type and length, and a
// CSP sandbox so the file can never run as a page.
func wantFile(test *testing.T, handler http.Handler, method, path string) {
	test.Helper()
	req := apitest.AsUser(ada)(httptest.NewRequestWithContext(test.Context(), method, path, nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	header := rec.Header()
	if rec.Code != 200 || rec.Body.String() != "RIFF" || header.Get("Content-Type") != "image/webp" || header.Get("Content-Length") != "4" ||
		!strings.Contains(header.Get("Content-Security-Policy"), "sandbox") {
		test.Fatalf("got %d %q %v", rec.Code, rec.Body, header)
	}
}

// An upload over MaxImageBytes is refused while it is read, before the
// service is called (the mock has no expectations).
func TestImageUploadOverTheLimitIs400(test *testing.T) {
	test.Parallel()
	_, handler := newImageRouter(test)
	res := apitest.Do(test, handler, apitest.AsUser(ada), "POST", r.Images, strings.Repeat("x", services.MaxImageBytes+1))
	wantError(test, res, 400, services.ErrImageTooLarge.Message)
}

// Invalid import bodies never reach the service and name the field.
func TestImageImportDecode(test *testing.T) {
	test.Parallel()
	for _, body := range []string{`{"url":""}`, `{"url":"ftp://example.com/a.png"}`, `{"url":"/relative.png"}`, `{"url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`} {
		_, handler := newImageRouter(test)
		res := apitest.Do(test, handler, apitest.AsUser(ada), "POST", r.ImageImport, body)
		if res.Status != 400 || res.Details["url"] == "" {
			test.Errorf("%.40q: %d %v", body, res.Status, res.Details)
		}
	}
}
