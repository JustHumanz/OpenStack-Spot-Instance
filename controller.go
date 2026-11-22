package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

const (
	ActiveVM = "ACTIVE"
)

type Server struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	Host      string  `json:"OS-EXT-SRV-ATTR:host"`
	TaskState *string `json:"OS-EXT-STS:task_state"`
	Flavor    struct {
		ID string `json:"id"`
	} `json:"flavor"`
}

type Hypervisor struct {
	ID      string `json:"id"`
	Name    string `json:"hypervisor_hostname"`
	HostIP  string `json:"host_ip"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Service struct {
		Host string `json:"host"`
	} `json:"service"`
}

type Host struct {
	Name      string
	Provider  string
	HostIP    string
	Inventory map[string]Inventory
	Usage     map[string]int
	Load      float64
}

type Controller struct {
	Compute, Placement *gophercloud.ServiceClient
	Settings           config
	Metrics            Metrics
	LastMove           map[string]time.Time
	PendingServer      string
	PendingSince       time.Time
	PendingAction      string
}

type migrationCandidate struct {
	Server                Server
	Source                Host
	Resources             map[string]int
	CanMigrate            bool
	HasUnmeasuredCapacity bool
	Destination           Host
}

func (controller *Controller) servers(ctx context.Context) ([]Server, error) {
	var servers []Server
	marker := ""
	for {
		query := url.Values{"all_tenants": {"1"}, "limit": {"1000"}, "tags": {"spot_instance:true"}}
		if marker != "" {
			query.Set("marker", marker)
		}
		var response struct {
			Servers []Server `json:"servers"`
		}
		_, err := controller.Compute.Get(ctx, controller.Compute.ServiceURL("servers", "detail")+"?"+query.Encode(), &response, nil)
		if err != nil {
			return nil, err
		}
		if len(response.Servers) == 0 {
			return servers, nil
		}
		servers = append(servers, response.Servers...)
		marker = response.Servers[len(response.Servers)-1].ID
	}
}

func (controller *Controller) hosts(ctx context.Context) ([]Host, error) {
	var hosts []Host
	marker := ""
	client := *controller.Compute
	client.Microversion = "2.68"
	for {
		query := url.Values{"limit": {"1000"}}
		if marker != "" {
			query.Set("marker", marker)
		}
		var response struct {
			Hypervisors []Hypervisor `json:"hypervisors"`
		}
		_, err := client.Get(ctx, client.ServiceURL("os-hypervisors", "detail")+"?"+query.Encode(), &response, nil)
		if err != nil {
			return nil, err
		}
		if len(response.Hypervisors) == 0 {
			return hosts, nil
		}
		for _, hypervisor := range response.Hypervisors {
			if hypervisor.ID == "" || hypervisor.Service.Host == "" || hypervisor.State != "up" || hypervisor.Status != "enabled" {
				continue
			}
			var inventory struct {
				Inventories map[string]Inventory `json:"inventories"`
			}
			var usage struct {
				Usages map[string]int `json:"usages"`
			}
			base := controller.Placement.ServiceURL("resource_providers", hypervisor.ID)
			if _, err := controller.Placement.Get(ctx, base+"/inventories", &inventory, nil); err != nil {
				log.Printf("skip host %s: inventory: %v", hypervisor.Service.Host, err)
				continue
			}
			if _, err := controller.Placement.Get(ctx, base+"/usages", &usage, nil); err != nil {
				log.Printf("skip host %s: usage: %v", hypervisor.Service.Host, err)
				continue
			}
			hosts = append(hosts, Host{Name: hypervisor.Service.Host, Provider: hypervisor.ID, HostIP: hypervisor.HostIP, Inventory: inventory.Inventories, Usage: usage.Usages})
		}
		marker = response.Hypervisors[len(response.Hypervisors)-1].ID
	}
}

func (controller *Controller) requirements(ctx context.Context, server Server, source Host) (map[string]int, error) {
	var response struct {
		Allocations map[string]struct {
			Resources map[string]int `json:"resources"`
		} `json:"allocations"`
	}
	_, err := controller.Placement.Get(ctx, controller.Placement.ServiceURL("allocations", server.ID), &response, nil)
	if err != nil {
		return nil, err
	}
	if len(response.Allocations) != 1 {
		return nil, fmt.Errorf("multi-provider or missing allocations")
	}
	allocation, ok := response.Allocations[source.Provider]
	if !ok {
		return nil, fmt.Errorf("allocation does not belong to source provider")
	}
	if allocation.Resources["VCPU"] <= 0 || allocation.Resources["MEMORY_MB"] <= 0 {
		return nil, fmt.Errorf("only shared VCPU instances are supported")
	}
	for resource := range allocation.Resources {
		if resource != "VCPU" && resource != "MEMORY_MB" && resource != "DISK_GB" {
			return nil, fmt.Errorf("unsupported resource %s", resource)
		}
	}
	return allocation.Resources, nil
}

func fits(host Host, resources map[string]int) bool {
	for resource, amount := range resources {
		if amount <= 0 {
			continue
		}
		inventory, exists := host.Inventory[resource]
		if !exists || !inventory.Fits(host.Usage[resource], amount) {
			return false
		}
	}
	return true
}

func (controller *Controller) serverIneligibility(server Server, source Host) string {
	if server.Host != source.Name {
		return fmt.Sprintf("host=%q does not match overloaded host=%q", server.Host, source.Name)
	}
	if server.Status != "ACTIVE" {
		return fmt.Sprintf("status=%q is not ACTIVE", server.Status)
	}
	if server.TaskState != nil {
		return fmt.Sprintf("task_state=%q is in progress", *server.TaskState)
	}
	if !controller.Settings.Flavors[server.Flavor.ID] {
		return fmt.Sprintf("flavor_id=%q is not allowlisted", server.Flavor.ID)
	}
	if time.Since(controller.LastMove[server.ID]) < controller.Settings.Cooldown {
		return "migration cooldown has not elapsed"
	}
	return ""
}

func (controller *Controller) pending(ctx context.Context) (bool, error) {
	if controller.PendingServer == "" {
		return false, nil
	}
	client := *controller.Compute
	client.Microversion = "2.68"
	if controller.PendingAction == "shelve" || controller.PendingAction == "offload" {
		var response struct {
			Server Server `json:"server"`
		}
		if _, err := client.Get(ctx, client.ServiceURL("servers", controller.PendingServer), &response, nil); err != nil {
			return true, err
		}
		switch response.Server.Status {
		case "ERROR":
			log.Printf("shelve/offload failed server=%s status=ERROR", controller.PendingServer)
			controller.PendingServer = ""
			controller.PendingAction = ""
			return false, nil
		case "SHELVED_OFFLOADED":
			log.Printf("spot server shelved and offloaded server=%s", controller.PendingServer)
			controller.PendingServer = ""
			controller.PendingAction = ""
			return false, nil
		case "SHELVED":
			if controller.PendingAction == "shelve" && response.Server.TaskState == nil {
				body := map[string]any{"shelveOffload": nil}
				if _, err := client.Post(ctx, client.ServiceURL("servers", controller.PendingServer, "action"), body, nil, nil); err != nil {
					return true, fmt.Errorf("offload shelved server %s: %w", controller.PendingServer, err)
				}
				controller.PendingAction = "offload"
				log.Printf("shelve completed; offload requested server=%s", controller.PendingServer)
			}
		}
		log.Printf("waiting for spot server action=%s server=%s status=%s", controller.PendingAction, controller.PendingServer, response.Server.Status)
		return true, nil
	}
	if controller.PendingAction != "migration" {
		return true, fmt.Errorf("unknown pending action %q", controller.PendingAction)
	}
	query := url.Values{"instance_uuid": {controller.PendingServer}, "migration_type": {"live-migration"}}
	var response struct {
		Migrations []struct {
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		} `json:"migrations"`
	}
	if _, err := client.Get(ctx, client.ServiceURL("os-migrations")+"?"+query.Encode(), &response, nil); err != nil {
		return true, err
	}
	for _, migration := range response.Migrations {
		created, err := time.Parse("2006-01-02T15:04:05.000000", migration.CreatedAt)
		if err != nil {
			created, err = time.Parse(time.RFC3339Nano, migration.CreatedAt)
		}
		if err != nil || created.Before(controller.PendingSince.Add(-5*time.Second)) {
			continue
		}
		switch migration.Status {
		case "completed", "error", "failed", "cancelled":
			log.Printf("migration finished server=%s status=%s", controller.PendingServer, migration.Status)
			controller.PendingServer = ""
			controller.PendingAction = ""
			return false, nil
		default:
			return true, nil
		}
	}
	return true, nil
}

func (controller *Controller) Cycle(ctx context.Context) error {
	if pending, err := controller.pending(ctx); err != nil {
		return fmt.Errorf("check pending migration: %w", err)
	} else if pending {
		log.Printf("waiting for migration server=%s", controller.PendingServer)
		return nil
	}
	hosts, err := controller.hosts(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	loads, err := controller.Metrics.Loads(ctx, hosts)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	available := make([]Host, 0, len(hosts))
	if len(hosts) == 0 {
		log.Print("no enabled, available compute hosts with Placement inventory")
	}
	for _, host := range hosts {
		if load, ok := loads[host.Name]; ok {
			host.Load = load
			role := "destination-eligible"
			if load >= controller.Settings.Threshold {
				role = "overloaded-source-only"
			}
			log.Printf("host=%s load=%.2f%% threshold=%.2f%% role=%s", host.Name, load, controller.Settings.Threshold, role)
			available = append(available, host)
		} else {
			log.Printf("skip host %s: missing, ambiguous, or stale metric for %q (Nova host_ip=%q)", host.Name, metricName(controller.Settings, host.Name), host.HostIP)
		}
	}

	sort.Slice(available, func(left, right int) bool { return available[left].Load > available[right].Load })
	servers, err := controller.servers(ctx)
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	log.Printf("Nova returned %d spot-tagged servers", len(servers))
	var candidates []migrationCandidate
	for _, source := range available {
		if source.Load < controller.Settings.Threshold {
			continue
		}
		eligibleServers := 0
		for _, server := range servers {
			// Only pass the active allow the migrate
			if server.Status != ActiveVM {
				continue
			}

			if reason := controller.serverIneligibility(server, source); reason != "" {
				log.Printf("skip spot server=%s %s", server.ID, reason)
				continue
			}
			eligibleServers++
			resources, err := controller.requirements(ctx, server, source)
			if err != nil {
				log.Printf("skip server %s: %v", server.ID, err)
				continue
			}
			candidate := migrationCandidate{Server: server, Source: source, Resources: resources}
			for _, destination := range hosts {
				if destination.Name == source.Name {
					continue
				}
				if fits(destination, resources) {
					if _, hasMetrics := loads[destination.Name]; !hasMetrics {
						candidate.HasUnmeasuredCapacity = true
					}
				}
			}
			for index := len(available) - 1; index >= 0; index-- {
				destination := available[index]
				if destination.Name == source.Name || destination.Load >= controller.Settings.Threshold || source.Load-destination.Load < controller.Settings.MinImprovement || !fits(destination, resources) {
					continue
				}
				candidate.CanMigrate = true
				candidate.Destination = destination
				break
			}
			candidates = append(candidates, candidate)
		}
		if eligibleServers == 0 {
			log.Printf("no active, allowlisted spot-tagged servers eligible on overloaded host=%s", source.Name)
		}
	}

	for _, candidate := range candidates {
		if candidate.CanMigrate {
			destination := candidate.Destination
			log.Printf("proposal server=%s source=%s source_load=%.1f destination=%s destination_load=%.1f resources=%v dry_run=%t", candidate.Server.ID, candidate.Source.Name, candidate.Source.Load, destination.Name, destination.Load, candidate.Resources, controller.Settings.DryRun)
			if controller.Settings.DryRun {
				return nil
			}
			client := *controller.Compute
			client.Microversion = "2.68"
			body := map[string]any{"os-migrateLive": map[string]any{"host": destination.Name, "block_migration": "auto"}}
			if _, err := client.Post(ctx, client.ServiceURL("servers", candidate.Server.ID, "action"), body, nil, nil); err != nil {
				return fmt.Errorf("migration request for %s: %w", candidate.Server.ID, err)
			}
			controller.PendingServer = candidate.Server.ID
			controller.PendingAction = "migration"
			controller.PendingSince = time.Now().UTC()
			controller.LastMove[candidate.Server.ID] = time.Now()
			log.Printf("migration accepted server=%s; completion is asynchronous", candidate.Server.ID)
			return nil
		}
		if !candidate.CanMigrate {
			if candidate.HasUnmeasuredCapacity {
				log.Printf("cannot shelve server=%s: a capacity-fitting host has no usable metrics", candidate.Server.ID)
				continue
			}
			if !controller.Settings.ShelveWhenNoDestination {
				log.Printf("no eligible migration destination for spot server=%s; automatic shelving is disabled", candidate.Server.ID)
				continue
			}
			log.Printf("shelve proposal server=%s source=%s resources=%v dry_run=%t", candidate.Server.ID, candidate.Source.Name, candidate.Resources, controller.Settings.DryRun)
			if controller.Settings.DryRun {
				return nil
			}
			client := *controller.Compute
			client.Microversion = "2.68"
			body := map[string]any{"shelve": nil}
			if _, err := client.Post(ctx, client.ServiceURL("servers", candidate.Server.ID, "action"), body, nil, nil); err != nil {
				return fmt.Errorf("shelve request for %s: %w", candidate.Server.ID, err)
			}
			controller.PendingServer = candidate.Server.ID
			controller.PendingAction = "shelve"
			controller.PendingSince = time.Now().UTC()
			controller.LastMove[candidate.Server.ID] = time.Now()
			log.Printf("shelve accepted server=%s; waiting for shelve then offload", candidate.Server.ID)
			return nil
		}
		log.Printf("no migration target below threshold=%.2f%% meets load improvement for server=%s; Placement capacity exists elsewhere", controller.Settings.Threshold, candidate.Server.ID)
	}
	log.Print("no migration proposed")
	return nil
}
