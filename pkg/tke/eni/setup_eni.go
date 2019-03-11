package eni

import (
	"fmt"
	"net"
	"time"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/dbdd4us/qcloudapi-sdk-go/metadata"
	log "github.com/golang/glog"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/wait"

	"git.code.oa.com/gaiastack/galaxy/pkg/utils/ips"
)

const (
	mainRouteTable = 254
	devPrefix      = "eth"

	setupPeriod = 30 * time.Second
)

type eniMeta struct {
	Primary     bool
	Mac         string
	GateWay     string
	PrimaryIp   string
	Mask        string
	LocalIpList []string
}

func SetupENIs(stopChan <-chan struct{}) error {
	// setup eni network
	log.Infof("setup eni network")

	// get eni list from metadata
	metaCli := metadata.NewMetaData(nil)

	log.Infof("wait for eni metadata binding")
	var eniMetaMap map[string]*eniMeta
	err := wait.PollUntil(setupPeriod, func() (done bool, err error) {
		eniMetaMap, err = getENIMetaMap(metaCli)
		if err != nil {
			log.Errorf("failed to get eniMetaMap, error %v", err)
			return false, err
		}
		if len(eniMetaMap) == 1 {
			log.Warning("wait for eni binding")
			return false, nil
		}
		return true, nil
	}, stopChan)
	if err != nil {
		return err
	}

	err = wait.PollUntil(setupPeriod, func() (done bool, err error) {
		retMap := make(map[string]*eniMeta)
		for mac, eni := range eniMetaMap {
			retMap[mac] = eni
		}
		err = ensureENINetwrok(retMap)
		if err != nil {
			return false, nil
		}
		return true, nil
	}, stopChan)
	if err != nil {
		return err
	}

	return nil
}

func ensureENINetwrok(eniMetaMap map[string]*eniMeta) error {
	linkList, err := netlink.LinkList()
	if err != nil {
		log.Errorf("failed to list link: %v", err)
		return fmt.Errorf("failed to list link: %v", err)
	}
	for _, link := range linkList {
		mac := link.Attrs().HardwareAddr.String()
		ifName := link.Attrs().Name
		eniMeta, ok := eniMetaMap[mac]
		if !ok {
			// skip other link
			log.Infof("skip link name %s, mac %s", ifName, mac)
			continue
		}

		if eniMeta.Primary {
			// skip primary eni
			log.Infof("skip eni name %s, mac %s", ifName, mac)
		} else {
			log.Infof("setup eni %s network", ifName)
			devIndex, err := getENIIndex(ifName)
			if err != nil {
				log.Errorf("failed to get eni %s index: %v", ifName, err)
				return fmt.Errorf("failed to get eni %s index: %v", ifName, err)
			}
			ip := net.IPNet{IP: net.ParseIP(eniMeta.PrimaryIp), Mask: ips.ParseIPv4Mask(eniMeta.Mask)}
			err = ensureENINetwork(ifName, devIndex, ip)
			if err != nil {
				log.Errorf("failed to setup eni %s network: %v", ifName, err)
				return err
			}
		}
		delete(eniMetaMap, mac)
	}
	if len(eniMetaMap) != 0 {
		for _, eni := range eniMetaMap {
			log.Errorf("failed to ensure eni %+v network", eni)
		}
		return fmt.Errorf("failed to ensure all eni network")
	}
	return nil
}

func ensureENINetwork(ifname string, eniTable int, primaryIp net.IPNet) error {
	log.Infof("setting up network for an eni with %s, primaryIp %v, route table %d",
		ifname, primaryIp, eniTable)

	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %v", ifname, err)
	}

	// set eni up
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up eni %s: %v", link.Attrs().Name, err)
	}

	// ensure eni route
	if err := ensureENIRoute(link, &primaryIp, eniTable); err != nil {
		return err
	}

	return nil
}

func ensureENIRoute(link netlink.Link, primaryIp *net.IPNet, eniTable int) error {
	linkIndex := link.Attrs().Index
	ipn := primaryIp.IP.Mask(primaryIp.Mask)
	gw := ip.NextIP(ipn)

	r := netlink.Route{
		LinkIndex: linkIndex,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        gw,
		Table:     eniTable,
		Flags:     int(netlink.FLAG_ONLINK),
	}

	err := netlink.RouteReplace(&r)
	if err != nil {
		return fmt.Errorf("failed to replace route %+v: %v", &r, err)
	}

	// remove the route that default out to eni-x out of main route table
	cidr := net.IPNet{IP: ipn, Mask: primaryIp.Mask}
	defaultRoute := netlink.Route{
		Dst:   &cidr,
		Src:   primaryIp.IP,
		Table: mainRouteTable,
		Scope: netlink.SCOPE_LINK,
	}

	if err := netlink.RouteDel(&defaultRoute); err != nil {
		if !IsNotExistsError(err) {
			return fmt.Errorf("failed to delete route - %v, source - %+v: %v", cidr, primaryIp, err)
		}
	}
	return nil
}