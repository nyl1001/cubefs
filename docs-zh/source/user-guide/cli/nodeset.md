# nodeset管理

## 列出所有 nodeset

```bash
cfs-cli nodeset list
```

## 获取单个 nodeset 的信息

```bash
cfs-cli nodeset info [NODESET ID]
```

## 更新某个 nodeset 的信息

```bash
cfs-cli nodeset update [NODESET ID] [flags]
```

```bash
Flags:
    --dataNodeSelector string   Set the node select policy(datanode) for specify nodeset
    -h, --help                      help for update
    --metaNodeSelector string   Set the node select policy(metanode) for specify nodeset
```