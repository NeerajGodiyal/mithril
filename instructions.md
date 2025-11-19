## Verifier mode:

```
go build ./cmd/mithril ; ./mithril verifier -s -p <snapshot.tar.zst> --incremental-snapshot-filename=“<incremental_snapshot.tar.zst>”  --out <output_acctsdb_dir>  -r <rpc_url> --num-replay-slots 200000 --txpar 64
```
example:
```
go build ./cmd/mithril ; ./mithril verifier -s -p /tmp/remote/snapshot-375335721-5p59vJFjgFbGy6TLDm9qyLRUK6S47FQZp75EVYzGhK9h.tar.zst --incremental-snapshot-filename="/tmp/remote/incremental-snapshot-375335721-375344654-HJj9KykhmajsmYfvbiNtd2nSwmmzCdba8Ty3RoYPReSP.tar.zst"  --out /mnt/accounts_db/  -r https://mainnet.helius-rpc.com/?api-key=4b9cc841-b2fe-4758-ae0f-1e08ffba684a --num-replay-slots 200000 --txpar 64
```

## live with catchup:
```
go build ./cmd/mithril ; ./mithril catchup --out <output_acctsdb_dir> -r <rpc_url> --txpar 96 --rpc-node-list <rpc_addr_list.txt>
```
example:
```
go build ./cmd/mithril ; ./mithril catchup --out /mnt/accounts_db/  -r https://mainnet.helius-rpc.com/?api-key=4b9cc841-b2fe-4758-ae0f-1e08ffba684a --txpar 96 --rpc-node-list /home/ubuntu/rpcs.txt
```
