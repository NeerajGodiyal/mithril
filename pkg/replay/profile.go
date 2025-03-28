package replay

import (
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"

	_ "net/http/pprof"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

func installProfilerAndSignalHandler(acctsDb *accountsdb.AccountsDb) *os.File {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	f, err := os.Create("../mithril.prof")
	if err != nil {
		panic("unable to create profile file")
	}

	pprof.StartCPUProfile(f)

	go func() {
		for {
			s := <-sigChan
			switch s {
			case syscall.SIGINT:
				{
					mlog.Log.Infof("signal received. shutting down mithril.")
					pprof.StopCPUProfile()
					f.Close()
					acctsDb.CloseDb()
					os.Exit(0)
				}
			}
		}
	}()

	return f
}
