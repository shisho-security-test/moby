package libnetwork

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/libnetwork/etchosts"
	"github.com/docker/libnetwork/osl"
	"github.com/docker/libnetwork/resolvconf"
	"github.com/docker/libnetwork/types"
)

// Sandbox provides the control over the network container entity. It is a one to one mapping with the container.
type Sandbox interface {
	// ID returns the ID of the sandbox
	ID() string
	// Key returns the sandbox's key
	Key() string
	// ContainerID returns the container id associated to this sandbox
	ContainerID() string
	// Labels returns the sandbox's labels
	Labels() map[string]interface{}
	// Statistics retrieves the interfaces' statistics for the sandbox
	Statistics() (map[string]*osl.InterfaceStatistics, error)
	// Delete destroys this container after detaching it from all connected endpoints.
	Delete() error
}

// SandboxOption is a option setter function type used to pass varios options to
// NewNetContainer method. The various setter functions of type SandboxOption are
// provided by libnetwork, they look like ContainerOptionXXXX(...)
type SandboxOption func(sb *sandbox)

func (sb *sandbox) processOptions(options ...SandboxOption) {
	for _, opt := range options {
		if opt != nil {
			opt(sb)
		}
	}
}

type epHeap []*endpoint

type sandbox struct {
	id          string
	containerID string
	config      containerConfig
	osSbox      osl.Sandbox
	controller  *controller
	refCnt      int
	endpoints   epHeap
	epPriority  map[string]int
	//hostsPath      string
	//resolvConfPath string
	joinLeaveDone chan struct{}
	sync.Mutex
}

// These are the container configs used to customize container /etc/hosts file.
type hostsPathConfig struct {
	hostName        string
	domainName      string
	hostsPath       string
	originHostsPath string
	extraHosts      []extraHost
	parentUpdates   []parentUpdate
}

type parentUpdate struct {
	cid  string
	name string
	ip   string
}

type extraHost struct {
	name string
	IP   string
}

// These are the container configs used to customize container /etc/resolv.conf file.
type resolvConfPathConfig struct {
	resolvConfPath       string
	originResolvConfPath string
	resolvConfHashFile   string
	dnsList              []string
	dnsSearchList        []string
}

type containerConfig struct {
	hostsPathConfig
	resolvConfPathConfig
	generic           map[string]interface{}
	useDefaultSandBox bool
	prio              int // higher the value, more the priority
}

func (sb *sandbox) ID() string {
	return sb.id
}

func (sb *sandbox) ContainerID() string {
	return sb.containerID
}

func (sb *sandbox) Key() string {
	if sb.config.useDefaultSandBox {
		return osl.GenerateKey("default")
	}
	return osl.GenerateKey(sb.id)
}

func (sb *sandbox) Labels() map[string]interface{} {
	return sb.config.generic
}

func (sb *sandbox) Statistics() (map[string]*osl.InterfaceStatistics, error) {
	m := make(map[string]*osl.InterfaceStatistics)

	if sb.osSbox == nil {
		return m, nil
	}

	var err error
	for _, i := range sb.osSbox.Info().Interfaces() {
		if m[i.DstName()], err = i.Statistics(); err != nil {
			return m, err
		}
	}

	return m, nil
}

func (sb *sandbox) Delete() error {
	sb.Lock()
	c := sb.controller
	eps := make([]*endpoint, len(sb.endpoints))
	for i, ep := range sb.endpoints {
		eps[i] = ep
	}
	sb.Unlock()

	// Detach from all containers
	for _, ep := range eps {
		if err := ep.Leave(sb); err != nil {
			log.Warnf("Failed detaching sandbox %s from endpoint %s: %v\n", sb.ID(), ep.ID(), err)
		}
	}

	if sb.osSbox != nil {
		sb.osSbox.Destroy()
	}

	c.Lock()
	delete(c.sandboxes, sb.ID())
	c.Unlock()

	return nil
}

func (sb *sandbox) MarshalJSON() ([]byte, error) {
	sb.Lock()
	defer sb.Unlock()

	// We are just interested in the container ID. This can be expanded to include all of containerInfo if there is a need
	return json.Marshal(sb.id)
}

func (sb *sandbox) UnmarshalJSON(b []byte) (err error) {
	sb.Lock()
	defer sb.Unlock()

	var id string
	if err := json.Unmarshal(b, &id); err != nil {
		return err
	}
	sb.id = id
	return nil
}

func (sb *sandbox) updateGateway(ep *endpoint) error {
	sb.osSbox.UnsetGateway()
	sb.osSbox.UnsetGatewayIPv6()

	if ep == nil {
		return nil
	}

	ep.Lock()
	joinInfo := ep.joinInfo
	ep.Unlock()

	if err := sb.osSbox.SetGateway(joinInfo.gw); err != nil {
		return fmt.Errorf("failed to set gateway while updating gateway: %v", err)
	}

	if err := sb.osSbox.SetGatewayIPv6(joinInfo.gw6); err != nil {
		return fmt.Errorf("failed to set IPv6 gateway while updating gateway: %v", err)
	}

	return nil
}

func (sb *sandbox) populateNetworkResources(ep *endpoint) error {
	ep.Lock()
	joinInfo := ep.joinInfo
	ifaces := ep.iFaces
	ep.Unlock()

	for _, i := range ifaces {
		var ifaceOptions []osl.IfaceOption

		ifaceOptions = append(ifaceOptions, sb.osSbox.InterfaceOptions().Address(&i.addr), sb.osSbox.InterfaceOptions().Routes(i.routes))
		if i.addrv6.IP.To16() != nil {
			ifaceOptions = append(ifaceOptions, sb.osSbox.InterfaceOptions().AddressIPv6(&i.addrv6))
		}

		if err := sb.osSbox.AddInterface(i.srcName, i.dstPrefix, ifaceOptions...); err != nil {
			return fmt.Errorf("failed to add interface %s to sandbox: %v", i.srcName, err)
		}
	}

	if joinInfo != nil {
		// Set up non-interface routes.
		for _, r := range joinInfo.StaticRoutes {
			if err := sb.osSbox.AddStaticRoute(r); err != nil {
				return fmt.Errorf("failed to add static route %s: %v", r.Destination.String(), err)
			}
		}
	}

	sb.Lock()
	heap.Push(&sb.endpoints, ep)
	highEp := sb.endpoints[0]
	sb.Unlock()
	if ep == highEp {
		if err := sb.updateGateway(ep); err != nil {
			return err
		}
	}

	return nil
}

func (sb *sandbox) clearNetworkResources(ep *endpoint) error {

	for _, i := range sb.osSbox.Info().Interfaces() {
		// Only remove the interfaces owned by this endpoint from the sandbox.
		if ep.hasInterface(i.SrcName()) {
			if err := i.Remove(); err != nil {
				log.Debugf("Remove interface failed: %v", err)
			}
		}
	}

	ep.Lock()
	joinInfo := ep.joinInfo
	ep.Unlock()

	// Remove non-interface routes.
	for _, r := range joinInfo.StaticRoutes {
		if err := sb.osSbox.RemoveStaticRoute(r); err != nil {
			log.Debugf("Remove route failed: %v", err)
		}
	}

	sb.Lock()
	if len(sb.endpoints) == 0 {
		// sb.endpoints should never be empty and this is unexpected error condition
		// We log an error message to note this down for debugging purposes.
		log.Errorf("No endpoints in sandbox while trying to remove endpoint %s", ep.Name())
		sb.Unlock()
		return nil
	}

	highEpBefore := sb.endpoints[0]
	var (
		i int
		e *endpoint
	)
	for i, e = range sb.endpoints {
		if e == ep {
			break
		}
	}
	heap.Remove(&sb.endpoints, i)
	var highEpAfter *endpoint
	if len(sb.endpoints) > 0 {
		highEpAfter = sb.endpoints[0]
	}
	delete(sb.epPriority, ep.ID())
	sb.Unlock()

	if highEpBefore != highEpAfter {
		sb.updateGateway(highEpAfter)
	}

	return nil
}

const (
	defaultPrefix = "/var/lib/docker/network/files"
	filePerm      = 0644
)

func (sb *sandbox) buildHostsFile() error {
	if sb.config.hostsPath == "" {
		sb.config.hostsPath = defaultPrefix + "/" + sb.id + "/hosts"
	}

	dir, _ := filepath.Split(sb.config.hostsPath)
	if err := createBasePath(dir); err != nil {
		return err
	}

	// This is for the host mode networking
	if sb.config.originHostsPath != "" {
		if err := copyFile(sb.config.originHostsPath, sb.config.hostsPath); err != nil && !os.IsNotExist(err) {
			return types.InternalErrorf("could not copy source hosts file %s to %s: %v", sb.config.originHostsPath, sb.config.hostsPath, err)
		}
		return nil
	}

	extraContent := make([]etchosts.Record, 0, len(sb.config.extraHosts))
	for _, extraHost := range sb.config.extraHosts {
		extraContent = append(extraContent, etchosts.Record{Hosts: extraHost.name, IP: extraHost.IP})
	}

	return etchosts.Build(sb.config.hostsPath, "", sb.config.hostName, sb.config.domainName, extraContent)
}

func (sb *sandbox) updateHostsFile(ifaceIP string, svcRecords []etchosts.Record) error {
	// Rebuild the hosts file accounting for the passed interface IP and service records
	extraContent := make([]etchosts.Record, 0, len(sb.config.extraHosts)+len(svcRecords))

	for _, extraHost := range sb.config.extraHosts {
		extraContent = append(extraContent, etchosts.Record{Hosts: extraHost.name, IP: extraHost.IP})
	}

	for _, svc := range svcRecords {
		extraContent = append(extraContent, svc)
	}

	return etchosts.Build(sb.config.hostsPath, ifaceIP, sb.config.hostName, sb.config.domainName, extraContent)
}

func (sb *sandbox) addHostsEntries(recs []etchosts.Record) {
	if err := etchosts.Add(sb.config.hostsPath, recs); err != nil {
		log.Warnf("Failed adding service host entries to the running container: %v", err)
	}
}

func (sb *sandbox) deleteHostsEntries(recs []etchosts.Record) {
	if err := etchosts.Delete(sb.config.hostsPath, recs); err != nil {
		log.Warnf("Failed deleting service host entries to the running container: %v", err)
	}
}

func (sb *sandbox) updateParentHosts() error {
	var pSb Sandbox

	for _, update := range sb.config.parentUpdates {
		sb.controller.WalkSandboxes(SandboxContainerWalker(&pSb, update.cid))
		if pSb == nil {
			continue
		}
		if err := etchosts.Update(pSb.(*sandbox).config.hostsPath, update.ip, update.name); err != nil {
			return err
		}
	}

	return nil
}

func (sb *sandbox) setupDNS() error {
	if sb.config.resolvConfPath == "" {
		sb.config.resolvConfPath = defaultPrefix + "/" + sb.id + "/resolv.conf"
	}

	sb.config.resolvConfHashFile = sb.config.resolvConfPath + ".hash"

	dir, _ := filepath.Split(sb.config.resolvConfPath)
	if err := createBasePath(dir); err != nil {
		return err
	}

	// This is for the host mode networking
	if sb.config.originResolvConfPath != "" {
		if err := copyFile(sb.config.originResolvConfPath, sb.config.resolvConfPath); err != nil {
			return fmt.Errorf("could not copy source resolv.conf file %s to %s: %v", sb.config.originResolvConfPath, sb.config.resolvConfPath, err)
		}
		return nil
	}

	resolvConf, err := resolvconf.Get()
	if err != nil {
		return err
	}
	dnsList := resolvconf.GetNameservers(resolvConf)
	dnsSearchList := resolvconf.GetSearchDomains(resolvConf)

	if len(sb.config.dnsList) > 0 || len(sb.config.dnsSearchList) > 0 {
		if len(sb.config.dnsList) > 0 {
			dnsList = sb.config.dnsList
		}
		if len(sb.config.dnsSearchList) > 0 {
			dnsSearchList = sb.config.dnsSearchList
		}
	}

	hash, err := resolvconf.Build(sb.config.resolvConfPath, dnsList, dnsSearchList)
	if err != nil {
		return err
	}

	// write hash
	if err := ioutil.WriteFile(sb.config.resolvConfHashFile, []byte(hash), filePerm); err != nil {
		return types.InternalErrorf("failed to write resol.conf hash file when setting up dns for sandbox %s: %v", sb.ID(), err)
	}

	return nil
}

func (sb *sandbox) updateDNS(ipv6Enabled bool) error {
	var oldHash []byte
	hashFile := sb.config.resolvConfHashFile

	resolvConf, err := ioutil.ReadFile(sb.config.resolvConfPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else {
		oldHash, err = ioutil.ReadFile(hashFile)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}

			oldHash = []byte{}
		}
	}

	curHash, err := ioutils.HashData(bytes.NewReader(resolvConf))
	if err != nil {
		return err
	}

	if string(oldHash) != "" && curHash != string(oldHash) {
		// Seems the user has changed the container resolv.conf since the last time
		// we checked so return without doing anything.
		log.Infof("Skipping update of resolv.conf file with ipv6Enabled: %t because file was touched by user", ipv6Enabled)
		return nil
	}

	// replace any localhost/127.* and remove IPv6 nameservers if IPv6 disabled.
	resolvConf, _ = resolvconf.FilterResolvDNS(resolvConf, ipv6Enabled)

	newHash, err := ioutils.HashData(bytes.NewReader(resolvConf))
	if err != nil {
		return err
	}

	// for atomic updates to these files, use temporary files with os.Rename:
	dir := path.Dir(sb.config.resolvConfPath)
	tmpHashFile, err := ioutil.TempFile(dir, "hash")
	if err != nil {
		return err
	}
	tmpResolvFile, err := ioutil.TempFile(dir, "resolv")
	if err != nil {
		return err
	}

	// Change the perms to filePerm (0644) since ioutil.TempFile creates it by default as 0600
	if err := os.Chmod(tmpResolvFile.Name(), filePerm); err != nil {
		return err
	}

	// write the updates to the temp files
	if err = ioutil.WriteFile(tmpHashFile.Name(), []byte(newHash), filePerm); err != nil {
		return err
	}
	if err = ioutil.WriteFile(tmpResolvFile.Name(), resolvConf, filePerm); err != nil {
		return err
	}

	// rename the temp files for atomic replace
	if err = os.Rename(tmpHashFile.Name(), hashFile); err != nil {
		return err
	}
	return os.Rename(tmpResolvFile.Name(), sb.config.resolvConfPath)
}

// OptionHostname function returns an option setter for hostname option to
// be passed to NewSandbox method.
func OptionHostname(name string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.hostName = name
	}
}

// OptionDomainname function returns an option setter for domainname option to
// be passed to NewSandbox method.
func OptionDomainname(name string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.domainName = name
	}
}

// OptionHostsPath function returns an option setter for hostspath option to
// be passed to NewSandbox method.
func OptionHostsPath(path string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.hostsPath = path
	}
}

// OptionOriginHostsPath function returns an option setter for origin hosts file path
// tbeo  passed to NewSandbox method.
func OptionOriginHostsPath(path string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.originHostsPath = path
	}
}

// OptionExtraHost function returns an option setter for extra /etc/hosts options
// which is a name and IP as strings.
func OptionExtraHost(name string, IP string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.extraHosts = append(sb.config.extraHosts, extraHost{name: name, IP: IP})
	}
}

// OptionParentUpdate function returns an option setter for parent container
// which needs to update the IP address for the linked container.
func OptionParentUpdate(cid string, name, ip string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.parentUpdates = append(sb.config.parentUpdates, parentUpdate{cid: cid, name: name, ip: ip})
	}
}

// OptionResolvConfPath function returns an option setter for resolvconfpath option to
// be passed to net container methods.
func OptionResolvConfPath(path string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.resolvConfPath = path
	}
}

// OptionOriginResolvConfPath function returns an option setter to set the path to the
// origin resolv.conf file to be passed to net container methods.
func OptionOriginResolvConfPath(path string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.originResolvConfPath = path
	}
}

// OptionDNS function returns an option setter for dns entry option to
// be passed to container Create method.
func OptionDNS(dns string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.dnsList = append(sb.config.dnsList, dns)
	}
}

// OptionDNSSearch function returns an option setter for dns search entry option to
// be passed to container Create method.
func OptionDNSSearch(search string) SandboxOption {
	return func(sb *sandbox) {
		sb.config.dnsSearchList = append(sb.config.dnsSearchList, search)
	}
}

// OptionUseDefaultSandbox function returns an option setter for using default sandbox to
// be passed to container Create method.
func OptionUseDefaultSandbox() SandboxOption {
	return func(sb *sandbox) {
		sb.config.useDefaultSandBox = true
	}
}

// OptionGeneric function returns an option setter for Generic configuration
// that is not managed by libNetwork but can be used by the Drivers during the call to
// net container creation method. Container Labels are a good example.
func OptionGeneric(generic map[string]interface{}) SandboxOption {
	return func(sb *sandbox) {
		sb.config.generic = generic
	}
}

func (eh epHeap) Len() int { return len(eh) }

func (eh epHeap) Less(i, j int) bool {
	ci, _ := eh[i].getSandbox()
	cj, _ := eh[j].getSandbox()

	cip, ok := ci.epPriority[eh[i].ID()]
	if !ok {
		cip = 0
	}
	cjp, ok := cj.epPriority[eh[j].ID()]
	if !ok {
		cjp = 0
	}
	if cip == cjp {
		return eh[i].getNetwork().Name() < eh[j].getNetwork().Name()
	}

	return cip > cjp
}

func (eh epHeap) Swap(i, j int) { eh[i], eh[j] = eh[j], eh[i] }

func (eh *epHeap) Push(x interface{}) {
	*eh = append(*eh, x.(*endpoint))
}

func (eh *epHeap) Pop() interface{} {
	old := *eh
	n := len(old)
	x := old[n-1]
	*eh = old[0 : n-1]
	return x
}

func createBasePath(dir string) error {
	return os.MkdirAll(dir, filePerm)
}

func createFile(path string) error {
	var f *os.File

	dir, _ := filepath.Split(path)
	err := createBasePath(dir)
	if err != nil {
		return err
	}

	f, err = os.Create(path)
	if err == nil {
		f.Close()
	}

	return err
}

func copyFile(src, dst string) error {
	sBytes, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(dst, sBytes, filePerm)
}
