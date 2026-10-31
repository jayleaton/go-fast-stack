// Command deploy-services prints the Railway app services platform.yaml
// gives an environment, in the order a deploy rolls them out
// (.github/scripts/deploy-railway.sh): web first, whose pre-deploy command
// migrates the app database, then admin (its own migration) when admin is
// on, then the backup cron when backups are on. It reads no network and no
// credentials; Deploy staging and Deploy production run it, so the
// workflows follow platform.yaml instead of naming services themselves.
//
//	deploy-services -config platform.yaml            # "web admin backup"
//	deploy-services -config platform.yaml -public    # "web admin": the services with a /healthz
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

func main() {
	config := flag.String("config", "platform.yaml", "path to platform.yaml")
	public := flag.Bool("public", false, "only the services with a public /healthz")
	flag.Parse()
	cfg, err := platform.Load(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy-services:", err)
		os.Exit(1)
	}
	services := cfg.PublicServices()
	if !*public && cfg.Backups.Enabled {
		services = append(services, platform.ServiceBackup)
	}
	fmt.Println(strings.Join(services, " "))
}
