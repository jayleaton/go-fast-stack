// Railway Infrastructure as Code entry point (Go authoring, beta). The
// Railway CLI evaluates this file for one environment at a time:
//
//	railway config plan  --file deploy/.railway/railway.go --environment staging
//	railway config apply --file deploy/.railway/railway.go --environment staging
//
// Do not edit the graph here: edit deploy/platform.yaml (what you want) or
// deploy/platform (how it maps to Railway). `make railway-plan` and
// `make railway-apply` run this for every environment plus the steps IaC
// cannot do (project, environments, domains, secrets, previews).
//
// The CLI compiles only this file plus a generated main, so everything else
// lives in the platform package.
package main

import (
	"os"

	railway "github.com/railwayapp/railway-go-sdk"

	"github.com/jayleaton/ai-patterns-go/deploy/platform"
)

// Railway is called by the CLI with the target environment.
func Railway(ctx railway.Context) platform.Project {
	path := os.Getenv("PLATFORM_CONFIG") // set by railway-setup; the CLI runs us from this directory
	if path == "" {
		path = "../platform.yaml"
	}
	cfg, err := platform.LoadResolved(path)
	if err != nil {
		panic(err)
	}
	// railway-setup deploys by upload until Railway can read the repo.
	cfg.ForceUpload = os.Getenv("PLATFORM_SOURCE") == "upload"
	p, err := cfg.Build(ctx.Environment)
	if err != nil {
		panic(err)
	}
	return p
}
