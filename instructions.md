## Verifier mode:

```
go build ./cmd/mithril ; ./mithril verifier --load-from-snapshot -p <snapshot.tar.zst> --incremental-snapshot="<incremental_snapshot.tar.zst>" --accounts-path <output_acctsdb_dir> -r <rpc_url> --num-slots <num_slots> --txpar 64
```
example:
```
go build ./cmd/mithril ; ./mithril verifier --load-from-snapshot -p /tmp/remote/snapshot-375335721-5p59vJFjgFbGy6TLDm9qyLRUK6S47FQZp75EVYzGhK9h.tar.zst --incremental-snapshot="/tmp/remote/incremental-snapshot-375335721-375344654-HJj9KykhmajsmYfvbiNtd2nSwmmzCdba8Ty3RoYPReSP.tar.zst" --accounts-path /mnt/accounts_db/ -r http://your_rpc_node --num-slots 200000 --txpar 64
```

## Live with catchup:

Using a config file (recommended):
```
go build ./cmd/mithril ; ./mithril catchup-rpc --config config.toml
```

Or with CLI flags:
```
go build ./cmd/mithril ; ./mithril catchup-rpc --accounts-path <output_acctsdb_dir> -r <rpc_url> --txpar 96
```
example:
```
go build ./cmd/mithril ; ./mithril catchup-rpc --accounts-path /mnt/accounts_db/ -r http://your_rpc_node --txpar 96
```
