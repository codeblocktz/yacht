package orchestrator

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
// arguments. Empty is no region, which is valid: an install in one place.
func ValidateRegion(region string) error { return ValidateLabelValue("region", region) }
