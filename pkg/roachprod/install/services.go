// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package install

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"

	"github.com/alessio/shellescape"
	"github.com/cockroachdb/cockroach/pkg/roachprod/config"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
)

//go:embed scripts/open_ports.sh
var openPortsScript string

type ServiceType string

const (
	// ServiceTypeSQL is the service type for SQL services on a node.
	ServiceTypeSQL ServiceType = "sql"
	// ServiceTypeUI is the service type for UI services on a node.
	ServiceTypeUI ServiceType = "ui"
)

// SystemTenantName is default system tenant name.
const SystemTenantName = "system"

type ServiceMode string

const (
	// ServiceModeShared is the service mode for services that are shared on a host process.
	ServiceModeShared ServiceMode = "shared"
	// ServiceModeExternal is the service mode for services that are run in a separate process.
	ServiceModeExternal ServiceMode = "external"
)

// SharedPriorityClass is the priority class used to indicate when a service is shared.
const SharedPriorityClass = 1000

// ServiceDesc describes a service running on a node.
type ServiceDesc struct {
	// TenantName is the name of the tenant that owns the service.
	TenantName string
	// ServiceType is the type of service.
	ServiceType ServiceType
	// ServiceMode is the mode of the service.
	ServiceMode ServiceMode
	// Node is the node the service is running on.
	Node Node
	// Port is the port the service is running on.
	Port int
}

// NodeServiceMap is a convenience type for mapping services by service type for each node.
type NodeServiceMap map[Node]map[ServiceType]*ServiceDesc

// ServiceDescriptors is a convenience type for a slice of service descriptors.
type ServiceDescriptors []ServiceDesc

// localClusterPortCache is a workaround for local clusters to prevent multiple
// nodes from using the same port when searching for open ports.
var localClusterPortCache struct {
	mu        syncutil.Mutex
	startPort int
}

// serviceDNSName returns the DNS name for a service in the standard SRV form.
func serviceDNSName(
	dnsProvider vm.DNSProvider, tenantName string, serviceType ServiceType, clusterName string,
) string {
	// An SRV record name must adhere to the standard form:
	// _service._proto.name.
	return fmt.Sprintf("_%s-%s._tcp.%s.%s", tenantName, serviceType, clusterName, dnsProvider.Domain())
}

// serviceNameComponents returns the tenant name and service type from a DNS
// name in the standard SRV form.
func serviceNameComponents(name string) (string, ServiceType, error) {
	nameParts := strings.Split(name, ".")
	if len(nameParts) < 2 {
		return "", "", errors.Newf("invalid DNS SRV name: %s", name)
	}

	serviceName := strings.TrimPrefix(nameParts[0], "_")
	splitIndex := strings.LastIndex(serviceName, "-")
	if splitIndex == -1 {
		return "", "", errors.Newf("invalid service name: %s", serviceName)
	}

	serviceTypeStr := serviceName[splitIndex+1:]
	var serviceType ServiceType
	switch {
	case serviceTypeStr == string(ServiceTypeSQL):
		serviceType = ServiceTypeSQL
	case serviceTypeStr == string(ServiceTypeUI):
		serviceType = ServiceTypeUI
	default:
		return "", "", errors.Newf("invalid service type: %s", serviceTypeStr)
	}
	return serviceName[:splitIndex], serviceType, nil
}

// DiscoverServices discovers services running on the given nodes. Services
// matching the tenant name and service type are returned. It's possible that
// more than one service can be returned for the given parameters if additional
// services of the same type are running for the same tenant.
func (c *SyncedCluster) DiscoverServices(
	nodes Nodes, tenantName string, serviceType ServiceType,
) (ServiceDescriptors, error) {
	// If no tenant name is specified, use the system tenant.
	if tenantName == "" {
		tenantName = SystemTenantName
	}
	mu := syncutil.Mutex{}
	records := make([]vm.DNSRecord, 0)
	err := vm.FanOutDNS(c.VMs, func(dnsProvider vm.DNSProvider, _ vm.List) error {
		service := fmt.Sprintf("%s-%s", tenantName, string(serviceType))
		r, lookupErr := dnsProvider.LookupSRVRecords(service, "tcp", c.Name)
		if lookupErr != nil {
			return lookupErr
		}
		mu.Lock()
		defer mu.Unlock()
		records = append(records, r...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	descriptors, err := c.dnsRecordsToServiceDescriptors(records)
	if err != nil {
		return nil, err
	}
	return descriptors.Filter(nodes), nil
}

// DiscoverService is a convenience method for discovering a single service. It
// returns the highest priority service returned by DiscoverServices. If no
// services are found, it returns a service descriptor with the default port for
// the service type.
func (c *SyncedCluster) DiscoverService(
	node Node, tenantName string, serviceType ServiceType,
) (ServiceDesc, error) {
	services, err := c.DiscoverServices([]Node{node}, tenantName, serviceType)
	if err != nil {
		return ServiceDesc{}, err
	}
	// If no services are found, attempt to discover a service for the system
	// tenant, and assume the service is shared.
	if len(services) == 0 {
		services, err = c.DiscoverServices([]Node{node}, SystemTenantName, serviceType)
		if err != nil {
			return ServiceDesc{}, err
		}
	}
	// Finally, fall back to the default ports if no services are found. This is
	// useful for backwards compatibility with clusters that were created before
	// the introduction of service discovery, or without a DNS provider.
	// TODO(Herko): Remove this once DNS support is fully functional.
	if len(services) == 0 {
		var port int
		switch serviceType {
		case ServiceTypeSQL:
			port = config.DefaultSQLPort
		case ServiceTypeUI:
			port = config.DefaultAdminUIPort
		default:
			return ServiceDesc{}, errors.Newf("invalid service type: %s", serviceType)
		}
		return ServiceDesc{
			ServiceType: serviceType,
			ServiceMode: ServiceModeShared,
			TenantName:  tenantName,
			Node:        node,
			Port:        port,
		}, nil
	}

	// If there are multiple services available select the first one.
	return services[0], err
}

// MapServices discovers all service types for a given tenant and maps it by
// node and service type.
func (c *SyncedCluster) MapServices(tenantName string) (NodeServiceMap, error) {
	sqlServices, err := c.DiscoverServices(c.Nodes, tenantName, ServiceTypeSQL)
	if err != nil {
		return nil, err
	}
	uiServices, err := c.DiscoverServices(c.Nodes, tenantName, ServiceTypeUI)
	if err != nil {
		return nil, err
	}
	serviceMap := make(NodeServiceMap)
	for _, node := range c.Nodes {
		serviceMap[node] = make(map[ServiceType]*ServiceDesc)
	}
	services := append(sqlServices, uiServices...)
	for _, service := range services {
		serviceMap[service.Node][service.ServiceType] = &service
	}
	return serviceMap, nil
}

// RegisterServices registers services with the DNS provider. This function is
// lenient and will not return an error if no DNS provider is available to
// register the service.
func (c *SyncedCluster) RegisterServices(services ServiceDescriptors) error {
	servicesByDNSProvider := make(map[string]ServiceDescriptors)
	for _, desc := range services {
		dnsProvider := c.VMs[desc.Node-1].DNSProvider
		if dnsProvider == "" {
			continue
		}
		servicesByDNSProvider[dnsProvider] = append(servicesByDNSProvider[dnsProvider], desc)
	}
	for dnsProviderName := range servicesByDNSProvider {
		return vm.ForDNSProvider(dnsProviderName, func(dnsProvider vm.DNSProvider) error {
			records := make([]vm.DNSRecord, 0)
			for _, desc := range servicesByDNSProvider[dnsProviderName] {
				name := serviceDNSName(dnsProvider, desc.TenantName, desc.ServiceType, c.Name)
				priority := 0
				if desc.ServiceMode == ServiceModeShared {
					priority = SharedPriorityClass
				}
				srvData := net.SRV{
					Target:   c.TargetDNSName(desc.Node),
					Port:     uint16(desc.Port),
					Priority: uint16(priority),
					Weight:   0,
				}
				records = append(records, vm.CreateSRVRecord(name, srvData))
			}
			err := dnsProvider.CreateRecords(records...)
			if err != nil {
				return err
			}
			return nil
		})
	}
	return nil
}

// Filter returns ServiceDescriptors with only the descriptors that match
// the given nodes.
func (d ServiceDescriptors) Filter(nodes Nodes) ServiceDescriptors {
	filteredDescriptors := make(ServiceDescriptors, 0)
	for _, descriptor := range d {
		if !nodes.Contains(descriptor.Node) {
			continue
		}
		filteredDescriptors = append(filteredDescriptors, descriptor)
	}
	return filteredDescriptors
}

// FindOpenPorts finds the requested number of open ports on the provided node.
func (c *SyncedCluster) FindOpenPorts(
	ctx context.Context, l *logger.Logger, node Node, startPort, count int,
) ([]int, error) {
	tpl, err := template.New("open_ports").
		Funcs(template.FuncMap{"shesc": func(i interface{}) string {
			return shellescape.Quote(fmt.Sprint(i))
		}}).
		Delims("#{", "#}").
		Parse(openPortsScript)
	if err != nil {
		return nil, err
	}

	var ports []int
	if c.IsLocal() {
		// For local clusters, we need to keep track of the ports we've already used
		// so that we don't use them again, when this function is called in
		// parallel. This does not protect against the case where concurrent calls
		// are made to roachprod to create local clusters.
		localClusterPortCache.mu.Lock()
		defer func() {
			nextPort := startPort
			if len(ports) > 0 {
				nextPort = ports[len(ports)-1]
			}
			localClusterPortCache.startPort = nextPort + 1
			localClusterPortCache.mu.Unlock()
		}()
		if localClusterPortCache.startPort > startPort {
			startPort = localClusterPortCache.startPort
		}
	}

	var buf strings.Builder
	if err := tpl.Execute(&buf, struct {
		StartPort int
		PortCount int
	}{
		StartPort: startPort,
		PortCount: count,
	}); err != nil {
		return nil, err
	}

	res, err := c.runCmdOnSingleNode(ctx, l, node, buf.String(), defaultCmdOpts("find-ports"))
	if err != nil {
		return nil, err
	}
	ports, err = stringToIntegers(strings.TrimSpace(res.CombinedOut))
	if err != nil {
		return nil, err
	}
	if len(ports) != count {
		return nil, errors.Errorf("expected %d ports, got %d", count, len(ports))
	}
	return ports, nil
}

// stringToIntegers converts a string of space-separated integers into a slice.
func stringToIntegers(str string) ([]int, error) {
	fields := strings.Fields(str)
	integers := make([]int, len(fields))
	for i, field := range fields {
		port, err := strconv.Atoi(field)
		if err != nil {
			return nil, err
		}
		integers[i] = port
	}
	return integers, nil
}

// dnsRecordsToServiceDescriptors converts a slice of DNS SRV records into a
// slice of ServiceDescriptors.
func (c *SyncedCluster) dnsRecordsToServiceDescriptors(
	records []vm.DNSRecord,
) (ServiceDescriptors, error) {
	// Map public DNS names to nodes.
	dnsNameToNode := make(map[string]Node)
	for idx := range c.VMs {
		node := Node(idx + 1)
		dnsNameToNode[c.TargetDNSName(node)] = node
	}
	// Parse SRV records into service descriptors.
	ports := make(ServiceDescriptors, 0)
	for _, record := range records {
		if record.Type != vm.SRV {
			continue
		}
		data, err := record.ParseSRVRecord()
		if err != nil {
			return nil, err
		}
		if _, ok := dnsNameToNode[data.Target]; !ok {
			continue
		}
		serviceMode := ServiceModeExternal
		if data.Priority >= SharedPriorityClass {
			serviceMode = ServiceModeShared
		}
		tenantName, serviceType, err := serviceNameComponents(record.Name)
		if err != nil {
			return nil, err
		}
		ports = append(ports, ServiceDesc{
			TenantName:  tenantName,
			ServiceType: serviceType,
			ServiceMode: serviceMode,
			Port:        int(data.Port),
			Node:        dnsNameToNode[data.Target],
		})
	}
	return ports, nil
}

func (c *SyncedCluster) TargetDNSName(node Node) string {
	cVM := c.VMs[node-1]
	postfix := ""
	if c.IsLocal() {
		// For local clusters the Public DNS is the same for all nodes, so we
		// need to add a postfix to make them unique.
		postfix = fmt.Sprintf("%d.", int(node))
	}
	// Targets always end with a period as per SRV record convention.
	return fmt.Sprintf("%s.%s", cVM.PublicDNS, postfix)
}
