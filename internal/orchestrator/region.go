package orchestrator

import (
	"fmt"
	"regexp"
)

// RegionLabel is the node label a region is carried in.
//
// A region is where an app lives: its pods, its volumes and its databases
// stay on the region's nodes. It is a broader place than a site — a region
// holds one or more sites (SiteLabel), and an app's replicas are spread
// across the sites of its own region.
//
// Kubernetes' well-known region label rather than one of ours, for the reason
// the site is the zone label: it is the one tooling already reads, storage
// drivers included, and NodeRestriction lets a kubelet set it on its own node
// when it joins.
const RegionLabel = "topology.kubernetes.io/region"

// ValidateRegion checks a region name: a label value, by the rule a pool and a
// site are held to, and for the same reason — it is written into the join
// command somebody pastes into a root shell, and into the installer's K3s
// arguments. Narrower than a label value, though: a DNS label, with no dots
// or underscores, because a region's name is what a wrapper names things
// after — its edge's DNS record, its StorageClass — and a dot there would be
// a subdomain, an underscore no name at all. Empty is no region, which is
// valid: an install in one place.
func ValidateRegion(region string) error {
	if err := ValidateLabelValue("region", region); err != nil {
		return err
	}
	if region != "" && !regionRE.MatchString(region) {
		return fmt.Errorf("region name must be lowercase letters, numbers and dashes, " +
			"starting and ending with a letter or number")
	}
	return nil
}

var regionRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
