// Package ova reads Open Virtualization Appliance (.ova) archives. An OVA is a
// tar archive holding an OVF descriptor (.ovf) plus one or more VMware disk
// images (.vmdk). This package exposes those disks as random-access readers so
// the rest of udf can walk their partitions and filesystems.
package ova
