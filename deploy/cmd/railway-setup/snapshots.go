package main

import (
	"context"
	"slices"
	"strings"
)

// volumeSnapshots sets Railway's own backup schedule (backups.volume_snapshots:
// daily, weekly, monthly) on every Postgres volume in env, idempotently.
// IaC cannot express it (docs/railway-go-iac-issues.md #19); Railway's API
// can, and changing it redeploys nothing. Not in platform.yaml: the
// schedule is left as it is. Existing snapshots are never deleted: an
// emptied schedule only stops new ones. Railway refusing it (the Hobby plan
// has no volume backups) becomes a next step, not a failed apply.
func (s *setup) volumeSnapshots(ctx context.Context, env string) error {
	want, managed := s.cfg.VolumeSnapshots(env)
	if !managed {
		return nil
	}
	if s.project == nil || s.project.Environments[env] == "" {
		s.change("%s: Railway volume snapshots %v on every Postgres volume", env, want)
		return nil
	}
	volumes, err := s.volumeInstances(ctx, env)
	if err != nil {
		return err
	}
	for _, database := range s.databases() {
		volume := volumes[s.project.Services[database]]
		if volume == "" {
			s.change("%s/%s Railway volume snapshots %v (once its volume exists)", env, database, want)
			continue
		}
		var current struct {
			List []struct {
				Kind string `json:"kind"`
			} `json:"volumeInstanceBackupScheduleList"`
		}
		if err := s.rw.gql(ctx, `query($id: String!) { volumeInstanceBackupScheduleList(volumeInstanceId: $id) { kind } }`,
			map[string]any{"id": volume}, &current); err != nil {
			return err
		}
		have := []string{}
		for _, schedule := range current.List {
			have = append(have, schedule.Kind)
		}
		slices.Sort(have)
		if slices.Equal(have, want) {
			continue
		}
		if s.apply {
			err := s.rw.gql(ctx, `mutation($id: String!, $kinds: [VolumeInstanceBackupScheduleKind!]!) { volumeInstanceBackupScheduleUpdate(volumeInstanceId: $id, kinds: $kinds) }`,
				map[string]any{"id": volume, "kinds": want}, nil)
			if err != nil {
				s.say("  ! %s/%s Railway volume snapshots: %v", env, database, err)
				s.next("Railway refused volume snapshots %v on %s/%s (they need the Pro plan): upgrade, or remove backups.volume_snapshots from platform.yaml", want, env, database)
				continue
			}
		}
		s.change("%s/%s Railway volume snapshots %s -> %s", env, database, scheduleText(have), scheduleText(want))
	}
	return nil
}

func scheduleText(kinds []string) string {
	if len(kinds) == 0 {
		return "none"
	}
	return strings.ToLower(strings.Join(kinds, ","))
}

// volumeInstances maps service id to its volume instance id in env.
func (s *setup) volumeInstances(ctx context.Context, env string) (map[string]string, error) {
	var res struct {
		Environment struct {
			VolumeInstances struct {
				Edges []struct {
					Node struct {
						ID        string `json:"id"`
						ServiceID string `json:"serviceId"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"volumeInstances"`
		} `json:"environment"`
	}
	if err := s.rw.gql(ctx, `query($env: String!) { environment(id: $env) { volumeInstances { edges { node { id serviceId } } } } }`,
		map[string]any{"env": s.project.Environments[env]}, &res); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, edge := range res.Environment.VolumeInstances.Edges {
		out[edge.Node.ServiceID] = edge.Node.ID
	}
	return out, nil
}
