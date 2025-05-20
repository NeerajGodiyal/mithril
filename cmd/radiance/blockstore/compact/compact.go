//go:build !lite

package compact

import (
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/linxGnu/grocksdb"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var Cmd = cobra.Command{
	Use:   "compact <rocksdb>",
	Short: "Compact RocksDB database",
	Args:  cobra.ExactArgs(1),
}

func init() {
	Cmd.Run = run
}

func run(_ *cobra.Command, args []string) {
	dbOpts := grocksdb.NewDefaultOptions()

	cfNames, err := grocksdb.ListColumnFamilies(dbOpts, args[0])
	if err != nil {
		klog.Exitf("Failed to list column families: %s", err)
	}
	cfOpts := make([]*grocksdb.Options, len(cfNames))
	for i := range cfOpts {
		cfOpts[i] = grocksdb.NewDefaultOptions()
	}

	db, cfs, err := grocksdb.OpenDbColumnFamilies(dbOpts, args[0], cfNames, cfOpts)
	if err != nil {
		klog.Exitf("Failed to open blockstore: %s", err)
	}
	defer db.Close()

	//mlog.Log.Debugf("Flushing WAL")
	if err := db.FlushWAL(true); err != nil {
		klog.Exitf("Failed to flush WAL: %s", err)
	}
	//mlog.Log.Debugf("Flushed WAL")

	for _, cf := range cfs {
		name := cf.Name()
		//mlog.Log.Debugf("Compacting %s", name)
		db.CompactRangeCF(cf, grocksdb.Range{})
		//mlog.Log.Debugf("Compacted %s", name)
	}

	//mlog.Log.Debugf("Done")
}
