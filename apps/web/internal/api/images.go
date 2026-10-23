package api

//go:generate go tool mockgen -source=images.go -destination=mocks/images.go -package=mocks

import (
	"context"
	"io"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// ImageService is the port these endpoints call (*services.ImageService).
type ImageService interface {
	Upload(ctx context.Context, actor services.Actor, data []byte) (services.Image, error)
	Import(ctx context.Context, actor services.Actor, in validate.CreateImageFromURL) (services.Image, error)
	Open(ctx context.Context, actor services.Actor, id string) (services.ImageFile, error)
	Delete(ctx context.Context, actor services.Actor, id string) error
}

var _ ImageService = (*services.ImageService)(nil)

// Images serves the signed-in user's images under /api/core/v1/images. The
// server mounts it only when object storage is configured. Import and delete
// are Endpoints (JSON in, envelope out); the upload takes the raw image as
// its body and the download streams the file, so those two are routes.
type Images struct {
	svc  ImageService
	gate *access.Gate

	importURL Endpoint[validate.CreateImageFromURL, services.Image]
	delete    Endpoint[NoInput, Success]
}

// NewImages returns the image endpoints.
func NewImages(svc ImageService, gate *access.Gate) *Images {
	images := &Images{svc: svc, gate: gate}
	routes := config.APIRoutes
	images.importURL = Endpoint[validate.CreateImageFromURL, services.Image]{"POST", routes.ImageImport, access.SignedIn, images.importFrom}
	images.delete = Endpoint[NoInput, Success]{"DELETE", routes.ImageByID, access.SignedIn, images.deleteOne}
	return images
}

// Routes registers the endpoints on mux.
func (images *Images) Routes(mux *http.ServeMux) {
	routes := config.APIRoutes
	mux.Handle("POST "+routes.Images, RouteLimit(images.gate, access.SignedIn, services.MaxImageBytes, images.upload))
	mux.Handle(images.importURL.Pattern(), images.importURL.Serve(images.gate))
	mux.Handle("GET "+routes.ImageByID, RouteFile(images.gate, access.SignedIn, images.get))
	mux.Handle(images.delete.Pattern(), images.delete.Serve(images.gate))
}

// upload takes the image as the raw request body (any Content-Type: the
// service sniffs the bytes).
func (images *Images) upload(request *http.Request, caller access.Caller) (services.Image, error) {
	data, err := io.ReadAll(request.Body)
	if isTooLarge(err) {
		return services.Image{}, services.ErrImageTooLarge
	} else if err != nil {
		return services.Image{}, err
	}
	return images.svc.Upload(request.Context(), caller.Actor(), data)
}

func (images *Images) importFrom(request *http.Request, caller access.Caller, in validate.CreateImageFromURL) (services.Image, error) {
	return images.svc.Import(request.Context(), caller.Actor(), in)
}

func (images *Images) get(request *http.Request, caller access.Caller) (File, error) {
	file, err := images.svc.Open(request.Context(), caller.Actor(), request.PathValue("id"))
	return File(file), err
}

func (images *Images) deleteOne(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := images.svc.Delete(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}
