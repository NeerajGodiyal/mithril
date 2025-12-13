## Verify Range mode (replay a fixed number of slots):

```
go build ./cmd/mithril ; ./mithril verify-range --load-from-snapshot -p <snapshot.tar.zst> --incremental-snapshot="<incremental_snapshot.tar.zst>" --accounts-path <output_acctsdb_dir> -r <rpc_url> --num-slots <num_slots> --txpar 64
```
example:
```
go build ./cmd/mithril ; ./mithril verify-range --load-from-snapshot -p /tmp/remote/snapshot-375335721-5p59vJFjgFbGy6TLDm9qyLRUK6S47FQZp75EVYzGhK9h.tar.zst --incremental-snapshot="/tmp/remote/incremental-snapshot-375335721-375344654-HJj9KykhmajsmYfvbiNtd2nSwmmzCdba8Ty3RoYPReSP.tar.zst" --accounts-path /mnt/accounts_db/ -r http://your_rpc_node --num-slots 200000 --txpar 64
```

## Verify Live mode (catchup and run indefinitely):

Using a config file (recommended):
```
go build ./cmd/mithril ; ./mithril verify-live --config config.toml
```

Or with CLI flags (supports multiple RPC endpoints for load balancing):
```
go build ./cmd/mithril ; ./mithril verify-live --accounts-path <output_acctsdb_dir> -r <rpc_url> --txpar 96
```
example with single endpoint:
```
go build ./cmd/mithril ; ./mithril verify-live --accounts-path /mnt/accounts_db/ -r http://your_rpc_node --txpar 96
```
example with multiple endpoints:
```
go build ./cmd/mithril ; ./mithril verify-live --accounts-path /mnt/accounts_db/ -r http://rpc1,http://rpc2,http://rpc3 --txpar 96
```

### Using Overcast block source:

Set block source to overcast in config.toml:
```toml
[block]
    source = "overcast"
    overcast_endpoint = "localhost:9000"
```

Or via CLI:
```
./mithril verify-live --config config.toml --block-source overcast --overcast-endpoint localhost:9000
```
