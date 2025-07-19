# RaxSQL

[A weekend project](https://www.supasaf.com/blog/general/phxsql) to learn and study the core "Fastest Majority" algorithm from [PhxSQL](https://github.com/Tencent/phxsql).

## 📖 What is this?
This is a simple implementation of a distributed SQL database using the Raft consensus algorithm, built to understand how PhxSQL's "Fastest Majority" optimization works in cross-region deployments.

Note: This is purely for educational purposes and research. Not intended for production use.

## 🎯 The "Fastest Majority" Concept
In a typical cross-region setup like "2 nodes in Guangzhou + 1 node in Shanghai", traditional consensus algorithms suffer from high latency because they need to wait for distant nodes to respond.

PhxSQL's clever solution: only wait for the fastest majority instead of all nodes.

**Example**:

```
Guangzhou: Node A (Leader), Node B  
Shanghai:  Node C

Write operation:
1. Node A sends request to both Node B and Node C
2. Node A gets quick response from Node B (~2ms)
3. Majority achieved (A + B), commit immediately  
4. Node C response arrives later (~30ms) but doesn't block the operation
```

This way, cross-region latency doesn't slow down your writes!

## 🚀 Quick Start

**Prerequisites**
- Go 1.19+
- That's it!

**Installation & Setup**

```shell
# 1. Install dependencies
go mod init raft-db
go get github.com/mattn/go-sqlite3

# 2. Start 3-node cluster
# Terminal 1
go run main.go node1 8001 /tmp/node1.db localhost:8002 localhost:8003

# Terminal 2  
go run main.go node2 8002 /tmp/node2.db localhost:8001 localhost:8003   

# Terminal 3
go run main.go node3 8003 /tmp/node3.db localhost:8001 localhost:8002

# 3. Test SQL operations
# Create table
curl -X POST http://localhost:8001/sql -d '{"sql":"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)"}'

# Insert data
curl -X POST http://localhost:8001/sql -d '{"sql":"INSERT INTO users (name) VALUES (?)", "args":["Alice"]}'
curl -X POST http://localhost:8001/sql -d '{"sql":"INSERT INTO users (name) VALUES (?)", "args":["Bob"]}'

# Query from any node
curl -X POST http://localhost:8002/sql -d '{"sql":"SELECT * FROM users"}'
curl -X POST http://localhost:8003/sql -d '{"sql":"SELECT * FROM users"}'

# Check cluster status
curl http://localhost:8001/status
```

## 📋 What's Implemented

- Basic Raft Algorithm: Leader election, log replication, consensus
- SQL Interface: Basic CREATE, INSERT, SELECT operations
- Fastest Majority: Writes complete when fastest majority responds
- SQLite Backend: Local storage with WAL mode
- HTTP API: Simple REST interface for testing
