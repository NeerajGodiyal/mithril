Start mithril with a pprof-port (e.g. 6060) configured.

Collect a 30 second trace:

```
curl "http://localhost:6060/debug/pprof/trace?seconds=30" > trace.out
```

Convert with this tool:

```
./cmd/trace2perfetto -input trace.out -output trace.binaryproto
```

View in ui.perfetto.dev.
