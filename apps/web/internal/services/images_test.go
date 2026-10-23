package services_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// Magic bytes per format; the codec is mocked, so a header is enough.
var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpegBytes = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'}
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00")
	webpBytes = []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")
	avifBytes = ftyp("avif", "mif1", "miaf")
	webpOut   = services.NormalizedImage{Data: []byte("RIFF-webp-out"), Width: 640, Height: 480}
	adaKey    = regexp.MustCompile(`^users/u1/images/([0-9a-f]{32})\.webp$`)
	imageID   = "0123456789abcdef0123456789abcdef"
	eve       = services.Actor{UserID: "u2", Email: "eve@example.com", SessionID: "s2"}
	nobody    = services.Actor{}
)

// ftyp is an ISO-BMFF ftyp box with a major brand and compatible brands.
func ftyp(major string, compatible ...string) []byte {
	box := []byte{0, 0, 0, byte(16 + 4*len(compatible))}
	box = append(box, "ftyp"+major+"\x00\x00\x00\x00"...)
	for _, brand := range compatible {
		box = append(box, brand...)
	}
	return append(box, "meta"...)
}

type imagePorts struct {
	store *mocks.MockObjectStore
	fetch *mocks.MockImageFetcher
	codec *mocks.MockImageCodec
}

func newImageService(test *testing.T) (*services.ImageService, imagePorts) {
	ctrl := gomock.NewController(test)
	ports := imagePorts{mocks.NewMockObjectStore(ctrl), mocks.NewMockImageFetcher(ctrl), mocks.NewMockImageCodec(ctrl)}
	return services.NewImageService(ports.store, ports.fetch, ports.codec), ports
}

// expectStored expects one normalise of format and one Put under Ada's
// prefix, and returns the key the service chose.
func expectStored(ports imagePorts, format services.ImageFormat) *string {
	key := new(string)
	ports.codec.EXPECT().Normalize(gomock.Any(), gomock.Any(), format, services.MaxImageSide, services.MaxImagePixels).Return(webpOut, nil)
	ports.store.EXPECT().Put(gomock.Any(), gomock.Cond(func(got string) bool { *key = got; return adaKey.MatchString(got) }), webpOut.Data, "image/webp").Return(nil)
	return key
}

func wantStoredImage(test *testing.T, img services.Image, key string) {
	test.Helper()
	match := adaKey.FindStringSubmatch(key)
	if match == nil || img.ID != match[1] {
		test.Fatalf("image id %q does not name key %q", img.ID, key)
	}
	want := services.Image{ID: match[1], ContentType: "image/webp", Width: 640, Height: 480, Size: len(webpOut.Data)}
	if img != want {
		test.Fatalf("image = %+v, want %+v", img, want)
	}
}

// The format comes from the bytes, never from a declared type; HEIC and
// other non-images are refused before the codec runs.
func TestImageUploadSniffsTheFormat(test *testing.T) {
	test.Parallel()
	cases := map[string]struct {
		data   []byte
		format services.ImageFormat // "" = refused as unsupported
	}{
		"jpeg":                    {jpegBytes, services.FormatJPEG},
		"png":                     {pngBytes, services.FormatPNG},
		"gif":                     {gifBytes, services.FormatGIF},
		"webp":                    {webpBytes, services.FormatWebP},
		"avif":                    {avifBytes, services.FormatAVIF},
		"avif sequence":           {ftyp("avis", "msf1"), services.FormatAVIF},
		"avif as compatible":      {ftyp("mif1", "avif"), services.FormatAVIF},
		"heic":                    {ftyp("heic", "mif1", "heic"), ""},
		"mp4":                     {ftyp("isom", "mp41"), ""},
		"ftyp box bigger than in": {append([]byte{0, 0, 0x10, 0}, "ftypavif\x00\x00\x00\x00"...), ""},
		"svg":                     {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), ""},
		"html":                    {[]byte("<!doctype html><img src=x>"), ""},
		"riff but not webp":       {[]byte("RIFF\x24\x00\x00\x00WAVEfmt "), ""},
		"empty":                   {nil, ""},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			svc, ports := newImageService(test)
			if tc.format == "" {
				_, err := svc.Upload(ctx, ada, tc.data)
				wantErr(test, err, services.ErrImageUnsupported)
				return
			}
			key := expectStored(ports, tc.format)
			img, err := svc.Upload(ctx, ada, tc.data)
			if err != nil {
				test.Fatal(err)
			}
			wantStoredImage(test, img, *key)
		})
	}
}

// Each upload gets a fresh random id: two uploads never share a key.
func TestImageUploadKeysAreUnique(test *testing.T) {
	test.Parallel()
	svc, ports := newImageService(test)
	firstKey, secondKey := expectStored(ports, services.FormatPNG), expectStored(ports, services.FormatPNG)
	first, _ := svc.Upload(ctx, ada, pngBytes)
	second, _ := svc.Upload(ctx, ada, pngBytes)
	if first.ID == second.ID || *firstKey == *secondKey {
		test.Fatalf("ids %q and %q (keys %q, %q) collide", first.ID, second.ID, *firstKey, *secondKey)
	}
}

func TestImageUploadRules(test *testing.T) {
	test.Parallel()
	tooBig := append(bytes.Clone(pngBytes), make([]byte, services.MaxImageBytes)...)
	cases := map[string]struct {
		actor  services.Actor
		data   []byte
		expect func(ports imagePorts)
		want   error
	}{
		"no actor":            {actor: nobody, data: pngBytes, want: services.ErrForbidden},
		"over the byte limit": {actor: ada, data: tooBig, want: services.ErrImageTooLarge},
		"too many pixels": {actor: ada, data: pngBytes, want: services.ErrImageTooManyPixels, expect: func(ports imagePorts) {
			ports.codec.EXPECT().Normalize(gomock.Any(), gomock.Any(), services.FormatPNG, gomock.Any(), gomock.Any()).Return(services.NormalizedImage{}, services.ErrImageTooManyPixels)
		}},
		"corrupt": {actor: ada, data: pngBytes, want: services.ErrImageInvalid, expect: func(ports imagePorts) {
			ports.codec.EXPECT().Normalize(gomock.Any(), gomock.Any(), services.FormatPNG, gomock.Any(), gomock.Any()).Return(services.NormalizedImage{}, services.ErrImageInvalid)
		}},
		"store fails": {actor: ada, data: pngBytes, want: errDB, expect: func(ports imagePorts) {
			ports.codec.EXPECT().Normalize(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(webpOut, nil)
			ports.store.EXPECT().Put(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errDB)
		}},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			svc, ports := newImageService(test)
			if tc.expect != nil {
				tc.expect(ports)
			}
			_, err := svc.Upload(ctx, tc.actor, tc.data)
			if errors.Is(tc.want, errDB) {
				wantInternal(test, err, errDB)
				return
			}
			wantErr(test, err, tc.want)
		})
	}
}

func TestImageImportFetchesUnderADeadline(test *testing.T) {
	test.Parallel()
	svc, ports := newImageService(test)
	const raw = "https://images.example.com/cat.avif?sig=abc"
	start := time.Now()
	ports.fetch.EXPECT().Fetch(gomock.Any(), raw, int64(services.MaxImageBytes)).DoAndReturn(func(ctx context.Context, _ string, _ int64) ([]byte, error) {
		dl, ok := ctx.Deadline()
		if !ok || dl.After(start.Add(services.ImageFetchTimeout+time.Second)) {
			test.Errorf("fetch deadline = %v (set %v), want within %v", dl, ok, services.ImageFetchTimeout)
		}
		return avifBytes, nil
	})
	key := expectStored(ports, services.FormatAVIF)
	img, err := svc.Import(ctx, ada, validate.CreateImageFromURL{URL: raw})
	if err != nil {
		test.Fatal(err)
	}
	wantStoredImage(test, img, *key)
}

// URLs the rule refuses never reach the fetcher (no expectation is set);
// addresses refused at dial time come back from it as ErrImageURLNotAllowed.
func TestImageImportRules(test *testing.T) {
	test.Parallel()
	fetched := func(data []byte, err error) func(ports imagePorts) {
		return func(ports imagePorts) {
			ports.fetch.EXPECT().Fetch(gomock.Any(), gomock.Any(), gomock.Any()).Return(data, err)
		}
	}
	cases := map[string]struct {
		actor  services.Actor
		url    string
		expect func(ports imagePorts)
		want   error
	}{
		"no actor":              {actor: nobody, url: "https://example.com/a.png", want: services.ErrForbidden},
		"file scheme":           {url: "file:///etc/passwd", want: services.ErrImageURLNotAllowed},
		"credentials":           {url: "https://admin:pw@example.com/a.png", want: services.ErrImageURLNotAllowed},
		"internal port":         {url: "http://example.com:6379/", want: services.ErrImageURLNotAllowed},
		"decimal loopback":      {url: "http://2130706433/a.png", want: services.ErrImageURLNotAllowed},
		"hex loopback":          {url: "http://0x7f.1/a.png", want: services.ErrImageURLNotAllowed},
		"unparseable":           {url: "http://[::1", want: services.ErrImageURLNotAllowed},
		"blocked at dial time":  {url: "https://rebind.example/a.png", expect: fetched(nil, services.ErrImageURLNotAllowed), want: services.ErrImageURLNotAllowed},
		"redirect to metadata":  {url: "https://example.com/r", expect: fetched(nil, services.ErrImageURLNotAllowed), want: services.ErrImageURLNotAllowed},
		"too large":             {url: "https://example.com/big.png", expect: fetched(nil, services.ErrImageTooLarge), want: services.ErrImageTooLarge},
		"fetch fails":           {url: "https://example.com/404.png", expect: fetched(nil, services.ErrImageFetchFailed), want: services.ErrImageFetchFailed},
		"fetcher over the cap":  {url: "https://example.com/a.png", expect: fetched(make([]byte, services.MaxImageBytes+1), nil), want: services.ErrImageTooLarge},
		"html served as png":    {url: "https://example.com/a.png", expect: fetched([]byte("<html><script>"), nil), want: services.ErrImageUnsupported},
		"svg (scriptable) body": {url: "https://example.com/a.svg", expect: fetched([]byte("<svg onload=alert(1)>"), nil), want: services.ErrImageUnsupported},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			svc, ports := newImageService(test)
			if tc.expect != nil {
				tc.expect(ports)
			}
			actor := tc.actor
			if name != "no actor" {
				actor = ada
			}
			_, err := svc.Import(ctx, actor, validate.CreateImageFromURL{URL: tc.url})
			wantErr(test, err, tc.want)
		})
	}
}

func TestImageOpen(test *testing.T) {
	test.Parallel()
	svc, ports := newImageService(test)
	body := io.NopCloser(strings.NewReader("webp"))
	ports.store.EXPECT().Get(gomock.Any(), "users/u1/images/"+imageID+".webp").Return(services.ImageFile{ContentType: "image/webp", Size: 4, Body: body}, nil)
	file, err := svc.Open(ctx, ada, imageID)
	if err != nil || file.Body != body || file.ContentType != "image/webp" {
		test.Fatalf("Open = %+v, %v", file, err)
	}
}

// Keys are namespaced by the actor: another user's id is looked up under
// the caller's own prefix (not found), and an id that is not one of ours
// never reaches the store (no expectation is set).
func TestImageOpenAndDeleteStayInTheActorsPrefix(test *testing.T) {
	test.Parallel()
	test.Run("other user's image", func(test *testing.T) {
		test.Parallel()
		svc, ports := newImageService(test)
		ports.store.EXPECT().Get(gomock.Any(), "users/u2/images/"+imageID+".webp").Return(services.ImageFile{}, services.ErrImageNotFound)
		_, err := svc.Open(ctx, eve, imageID)
		wantErr(test, err, services.ErrImageNotFound)
		ports.store.EXPECT().Delete(gomock.Any(), "users/u2/images/"+imageID+".webp").Return(nil)
		if err := svc.Delete(ctx, eve, imageID); err != nil {
			test.Fatal(err)
		}
	})
	for _, id := range []string{"", "x", "../u1/images/" + imageID, imageID + ".webp", strings.ToUpper(imageID), imageID[:31] + "/", imageID + "0"} {
		test.Run("id "+id, func(test *testing.T) {
			test.Parallel()
			svc, _ := newImageService(test)
			_, err := svc.Open(ctx, ada, id)
			wantErr(test, err, services.ErrImageNotFound)
			wantErr(test, svc.Delete(ctx, ada, id), services.ErrImageNotFound)
		})
	}
	test.Run("no actor", func(test *testing.T) {
		test.Parallel()
		svc, _ := newImageService(test)
		_, err := svc.Open(ctx, nobody, imageID)
		wantErr(test, err, services.ErrForbidden)
		wantErr(test, svc.Delete(ctx, nobody, imageID), services.ErrForbidden)
	})
}

func TestImageDelete(test *testing.T) {
	test.Parallel()
	svc, ports := newImageService(test)
	key := "users/u1/images/" + imageID + ".webp"
	ports.store.EXPECT().Delete(gomock.Any(), key).Return(nil)
	if err := svc.Delete(ctx, ada, imageID); err != nil {
		test.Fatal(err)
	}
	ports.store.EXPECT().Delete(gomock.Any(), key).Return(errDB)
	wantInternal(test, svc.Delete(ctx, ada, imageID), errDB)
	ports.store.EXPECT().Get(gomock.Any(), key).Return(services.ImageFile{}, errDB)
	_, err := svc.Open(ctx, ada, imageID)
	if !errors.Is(err, errDB) {
		test.Fatalf("Open err = %v", err)
	}
}
