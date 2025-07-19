package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Node states
type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// SQL command types
type CommandType string

const (
	CmdSelect CommandType = "SELECT"
	CmdInsert CommandType = "INSERT"
	CmdUpdate CommandType = "UPDATE"
	CmdDelete CommandType = "DELETE"
	CmdCreate CommandType = "CREATE"
	CmdDrop   CommandType = "DROP"
)

// Log entry for SQL operations
type LogEntry struct {
	Term      int           `json:"term"`
	Index     int           `json:"index"`
	Command   CommandType   `json:"command"`
	SQL       string        `json:"sql"`
	Args      []interface{} `json:"args"`
	Timestamp int64         `json:"timestamp"`
}

// Database interface for different backends
type Database interface {
	Execute(sql string, args ...interface{}) (*QueryResult, error)
	Query(sql string, args ...interface{}) (*QueryResult, error)
	BeginTx() (Transaction, error)
	Close() error
}

type Transaction interface {
	Execute(sql string, args ...interface{}) (*QueryResult, error)
	Query(sql string, args ...interface{}) (*QueryResult, error)
	Commit() error
	Rollback() error
}

type QueryResult struct {
	Rows         []map[string]interface{} `json:"rows"`
	RowsAffected int64                    `json:"rows_affected"`
	LastInsertId int64                    `json:"last_insert_id"`
	Error        string                   `json:"error,omitempty"`
}

// SQLite implementation
type SQLiteDB struct {
	db *sql.DB
	mu sync.RWMutex
}

type SQLiteTx struct {
	tx *sql.Tx
}

func NewSQLiteDB(path string) (*SQLiteDB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	return &SQLiteDB{db: db}, nil
}

func (s *SQLiteDB) Execute(sqlStr string, args ...interface{}) (*QueryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(sqlStr, args...)
	if err != nil {
		return &QueryResult{Error: err.Error()}, err
	}
	rowsAffected, _ := result.RowsAffected()
	lastInsertId, _ := result.LastInsertId()
	return &QueryResult{
		RowsAffected: rowsAffected,
		LastInsertId: lastInsertId,
	}, nil
}

func (s *SQLiteDB) Query(sqlStr string, args ...interface{}) (*QueryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(sqlStr, args...)
	if err != nil {
		return &QueryResult{Error: err.Error()}, err
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	result := &QueryResult{Rows: make([]map[string]interface{}, 0)}
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		rows.Scan(valuePtrs...)
		row := make(map[string]interface{})
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = val
			}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func (s *SQLiteDB) BeginTx() (Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &SQLiteTx{tx: tx}, nil
}

func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

func (t *SQLiteTx) Execute(sqlStr string, args ...interface{}) (*QueryResult, error) {
	result, err := t.tx.Exec(sqlStr, args...)
	if err != nil {
		return &QueryResult{Error: err.Error()}, err
	}
	rowsAffected, _ := result.RowsAffected()
	lastInsertId, _ := result.LastInsertId()
	return &QueryResult{
		RowsAffected: rowsAffected,
		LastInsertId: lastInsertId,
	}, nil
}

func (t *SQLiteTx) Query(sqlStr string, args ...interface{}) (*QueryResult, error) {
	rows, err := t.tx.Query(sqlStr, args...)
	if err != nil {
		return &QueryResult{Error: err.Error()}, err
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	result := &QueryResult{Rows: make([]map[string]interface{}, 0)}
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		rows.Scan(valuePtrs...)
		row := make(map[string]interface{})
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = val
			}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func (t *SQLiteTx) Commit() error {
	return t.tx.Commit()
}

func (t *SQLiteTx) Rollback() error {
	return t.tx.Rollback()
}

// Raft consensus node
type RaftNode struct {
	mu          sync.RWMutex
	id          string
	state       NodeState
	term        int
	votedFor    string
	log         []LogEntry
	commitIndex int
	lastApplied int
	nextIndex   map[string]int
	matchIndex  map[string]int
	peers       []string
	db          Database
	appendCh    chan bool
	voteCh      chan bool
	server      *http.Server
}

// Raft RPC structures
type VoteRequest struct {
	Term         int    `json:"term"`
	CandidateId  string `json:"candidate_id"`
	LastLogIndex int    `json:"last_log_index"`
	LastLogTerm  int    `json:"last_log_term"`
}

type VoteResponse struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"vote_granted"`
}

type AppendRequest struct {
	Term         int        `json:"term"`
	LeaderId     string     `json:"leader_id"`
	PrevLogIndex int        `json:"prev_log_index"`
	PrevLogTerm  int        `json:"prev_log_term"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leader_commit"`
}

type AppendResponse struct {
	Term    int  `json:"term"`
	Success bool `json:"success"`
}

// Client SQL request
type SQLRequest struct {
	SQL  string        `json:"sql"`
	Args []interface{} `json:"args"`
}

type SQLResponse struct {
	Success bool         `json:"success"`
	Result  *QueryResult `json:"result,omitempty"`
	Error   string       `json:"error,omitempty"`
	Leader  string       `json:"leader,omitempty"`
}

func NewRaftNode(id string, peers []string, port int, dbPath string) (*RaftNode, error) {
	db, err := NewSQLiteDB(dbPath)
	if err != nil {
		return nil, err
	}
	node := &RaftNode{
		id:          id,
		state:       Follower,
		term:        0,
		votedFor:    "",
		log:         make([]LogEntry, 0),
		commitIndex: -1,
		lastApplied: -1,
		nextIndex:   make(map[string]int),
		matchIndex:  make(map[string]int),
		peers:       peers,
		db:          db,
		appendCh:    make(chan bool, 1),
		voteCh:      make(chan bool, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/vote", node.handleVote)
	mux.HandleFunc("/append", node.handleAppend)
	mux.HandleFunc("/sql", node.handleSQL)
	mux.HandleFunc("/status", node.handleStatus)
	node.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	return node, nil
}

func (n *RaftNode) Start() {
	go n.server.ListenAndServe()
	go n.run()
	go n.applyCommittedEntries()
}

func (n *RaftNode) run() {
	for {
		switch n.state {
		case Follower:
			n.runFollower()
		case Candidate:
			n.runCandidate()
		case Leader:
			n.runLeader()
		}
	}
}

func (n *RaftNode) runFollower() {
	timeout := time.Duration(300+rand.Intn(200)) * time.Millisecond
	select {
	case <-n.appendCh:
	case <-n.voteCh:
	case <-time.After(timeout):
		n.mu.Lock()
		n.state = Candidate
		n.mu.Unlock()
		log.Printf("Node %s: Election timeout occurred, become a Candidate", n.id)
	}
}

func (n *RaftNode) runCandidate() {
	n.mu.Lock()
	n.term++
	n.votedFor = n.id
	currentTerm := n.term
	lastLogIndex := len(n.log) - 1
	lastLogTerm := 0
	if lastLogIndex >= 0 {
		lastLogTerm = n.log[lastLogIndex].Term
	}
	n.mu.Unlock()

	log.Printf("Node %s: Starting election for term %d", n.id, currentTerm)
	votes := 1
	var mu sync.Mutex
	done := make(chan bool, 1)

	for _, peer := range n.peers {
		go func(peer string) {
			req := VoteRequest{
				Term:         currentTerm,
				CandidateId:  n.id,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  lastLogTerm,
			}
			if resp, err := n.sendVoteRequest(peer, req); err == nil {
				mu.Lock()
				defer mu.Unlock()
				if resp.VoteGranted {
					votes++
					log.Printf("Node %s: Received vote from %s", n.id, peer)
				}
				if resp.Term > currentTerm {
					n.mu.Lock()
					n.term = resp.Term
					n.votedFor = ""
					n.state = Follower
					n.mu.Unlock()
					select {
					case done <- true:
					default:
					}
					return
				}
				if votes > len(n.peers)/2 {
					n.mu.Lock()
					if n.state == Candidate && n.term == currentTerm {
						n.state = Leader
						log.Printf("Node %s: Elected as Leader in term %d with %d votes", n.id, currentTerm, votes)
					}
					n.mu.Unlock()
					select {
					case done <- true:
					default:
					}
					return
				}
			} else {
				log.Printf("Node %s: Failed to send vote request to %s: %v", n.id, peer, err)
			}
		}(peer)
	}
	timeout := time.Duration(300+rand.Intn(200)) * time.Millisecond
	select {
	case <-done:
		return
	case <-n.appendCh:
		n.mu.Lock()
		n.state = Follower
		n.mu.Unlock()
		return
	case <-time.After(timeout):
		log.Printf("Node %s: Election timeout, received %d votes", n.id, votes)
		return
	}
}

func (n *RaftNode) runLeader() {
	n.mu.Lock()
	log.Printf("Node %s: Starting as Leader for term %d", n.id, n.term)
	for _, peer := range n.peers {
		n.nextIndex[peer] = len(n.log)
		n.matchIndex[peer] = -1
	}
	n.mu.Unlock()

	n.sendHeartbeat()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if n.state != Leader {
			return
		}
		select {
		case <-ticker.C:
			n.sendHeartbeat()
		case <-n.appendCh:
			n.mu.Lock()
			n.state = Follower
			n.mu.Unlock()
			log.Printf("Node %s: Stepping down from Leader role", n.id)
			return
		}
	}
}

func (n *RaftNode) sendHeartbeat() {
	n.mu.RLock()
	currentTerm := n.term
	n.mu.RUnlock()

	for _, peer := range n.peers {
		go func(peer string) {
			n.mu.RLock()
			prevLogIndex := n.nextIndex[peer] - 1
			prevLogTerm := 0
			if prevLogIndex >= 0 && prevLogIndex < len(n.log) {
				prevLogTerm = n.log[prevLogIndex].Term
			}
			entries := make([]LogEntry, 0)
			if len(n.log) > n.nextIndex[peer] {
				entries = n.log[n.nextIndex[peer]:]
			}
			req := AppendRequest{
				Term:         currentTerm,
				LeaderId:     n.id,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      entries,
				LeaderCommit: n.commitIndex,
			}
			n.mu.RUnlock()

			if resp, err := n.sendAppendRequest(peer, req); err == nil {
				n.mu.Lock()
				if resp.Term > n.term {
					n.term = resp.Term
					n.votedFor = ""
					n.state = Follower
					n.mu.Unlock()
					return
				}
				if n.state == Leader && n.term == currentTerm {
					if resp.Success {
						n.nextIndex[peer] = prevLogIndex + len(entries) + 1
						n.matchIndex[peer] = prevLogIndex + len(entries)
						n.updateCommitIndex()
					} else {
						if n.nextIndex[peer] > 0 {
							n.nextIndex[peer]--
						}
					}
				}
				n.mu.Unlock()
			} else {
				// No need to log here to avoid log flooding during network partition
			}
		}(peer)
	}
}

func (n *RaftNode) updateCommitIndex() {
	// This function is currently only called by the background heartbeat to synchronize the follower's commitIndex.
	matches := make([]int, 0)
	for _, index := range n.matchIndex {
		matches = append(matches, index)
	}
	matches = append(matches, len(n.log)-1)
	sort.Ints(matches)
	majorityIndex := (len(matches) - 1) / 2
	newCommitIndex := matches[majorityIndex]
	if newCommitIndex > n.commitIndex {
		if newCommitIndex < len(n.log) && n.log[newCommitIndex].Term == n.term {
			n.commitIndex = newCommitIndex
		}
	}
}

func (n *RaftNode) applyCommittedEntries() {
	for {
		time.Sleep(10 * time.Millisecond)
		var entriesToApply []LogEntry

		n.mu.Lock()
		if n.lastApplied < n.commitIndex {
			start := n.lastApplied + 1
			end := n.commitIndex + 1
			entriesToApply = n.log[start:end]
			n.lastApplied = n.commitIndex
		}
		n.mu.Unlock()

		for _, entry := range entriesToApply {
			n.applyEntry(entry)
		}
	}
}

func (n *RaftNode) applyEntry(entry LogEntry) *QueryResult {
	log.Printf("Node %s: Entry Index %d, SQL: %s", n.id, entry.Index, entry.SQL)
	if isReadOnly(entry.Command) {
		return &QueryResult{Error: "Read-only commands should not appear in the log."}
	}
	result, err := n.db.Execute(entry.SQL, entry.Args...)
	if err != nil {
		log.Printf("Node %s: Error applying SQL: %v", n.id, err)
		return &QueryResult{Error: err.Error()}
	}
	return result
}

func isReadOnly(cmd CommandType) bool {
	return cmd == CmdSelect
}

func parseCommand(sql string) CommandType {
	sql = strings.TrimSpace(strings.ToUpper(sql))
	if strings.HasPrefix(sql, "SELECT") {
		return CmdSelect
	} else if strings.HasPrefix(sql, "INSERT") {
		return CmdInsert
	} else if strings.HasPrefix(sql, "UPDATE") {
		return CmdUpdate
	} else if strings.HasPrefix(sql, "DELETE") {
		return CmdDelete
	} else if strings.HasPrefix(sql, "CREATE") {
		return CmdCreate
	} else if strings.HasPrefix(sql, "DROP") {
		return CmdDrop
	}
	return CmdSelect
}

func (n *RaftNode) replicateAndWaitForMajority(entry LogEntry) bool {
	n.mu.RLock()
	currentTerm := n.term
	peers := n.peers
	n.mu.RUnlock()

	majorityNeeded := (len(peers) + 1) / 2

	var successCount struct {
		sync.Mutex
		count int
	}
	successCount.count = 1

	doneChan := make(chan bool, 1)

	// Send requests to followers
	for _, peer := range peers {
		go func(peer string) {
			n.mu.RLock()
			// Always send all logs starting from nextIndex to simplify the logic.
			// This can be optimized to send only new logs under high load.
			prevLogIndex := n.nextIndex[peer] - 1
			prevLogTerm := 0
			if prevLogIndex >= 0 && prevLogIndex < len(n.log) {
				prevLogTerm = n.log[prevLogIndex].Term
			}
			entries := n.log[n.nextIndex[peer]:]
			req := AppendRequest{
				Term:         currentTerm,
				LeaderId:     n.id,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      entries,
				LeaderCommit: n.commitIndex,
			}
			n.mu.RUnlock()

			resp, err := n.sendAppendRequest(peer, req)
			if err != nil {
				return
			}

			n.mu.Lock()
			if resp.Term > n.term {
				n.term = resp.Term
				n.votedFor = ""
				n.state = Follower
				n.mu.Unlock()
				return
			}

			if resp.Success {
				n.nextIndex[peer] = entry.Index + 1
				n.matchIndex[peer] = entry.Index

				successCount.Lock()
				successCount.count++
				isMajority := successCount.count > majorityNeeded
				successCount.Unlock()

				if isMajority {
					select {
					case doneChan <- true: // Send Succ signal
					default:
					}
				}
			} else {
				// If it fails, decrease nextIndex to retry next time.
				if n.nextIndex[peer] > 0 {
					n.nextIndex[peer]--
				}
			}
			n.mu.Unlock()

		}(peer)
	}

	// Wait for majority confirmation or timeout.
	select {
	case <-doneChan:
		return true
	case <-time.After(2 * time.Second):
		log.Printf("Node %s: Waited for majority confirmation, but timed out.", n.id)
		return false
	}
}

func (n *RaftNode) handleSQL(w http.ResponseWriter, r *http.Request) {
	var req SQLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cmd := parseCommand(req.SQL)
	resp := SQLResponse{}

	if isReadOnly(cmd) {
		// Read-only operations are queried directly locally.
		// For strong consistency, one can check Leader identity and compare with commitIndex.
		result, err := n.db.Query(req.SQL, req.Args...)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Result = result
		}
	} else {
		// Write operations must be handled by the Leader.
		n.mu.RLock()
		if n.state != Leader {
			n.mu.RUnlock()
			resp.Error = "不是 Leader"
			// You can return the known Leader ID here.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		entry := LogEntry{
			Term:      n.term,
			Command:   cmd,
			SQL:       req.SQL,
			Args:      req.Args,
			Timestamp: time.Now().UnixNano(),
		}
		n.mu.RUnlock()

		n.mu.Lock()
		entry.Index = len(n.log)
		n.log = append(n.log, entry)
		log.Printf("Node %s: Add new entry %d (SQL: %s)", n.id, entry.Index, entry.SQL)
		n.mu.Unlock()

		ok := n.replicateAndWaitForMajority(entry)

		if ok {
			n.mu.Lock()
			if entry.Index > n.commitIndex {
				n.commitIndex = entry.Index
				log.Printf("Node %s: Commit index updated to %d", n.id, n.commitIndex)
			}

			if n.lastApplied < n.commitIndex {
				n.lastApplied = n.commitIndex
				n.mu.Unlock()
				result := n.applyEntry(entry)
				resp.Success = result.Error == ""
				resp.Result = result
				if !resp.Success {
					resp.Error = result.Error
				}
			} else {
				n.mu.Unlock()
				// This is rare but can happen.
				// It indicates the log has already been applied in the background, so we just report success.
				resp.Success = true
			}
		} else {
			resp.Error = "Operation timed out or failed to gain majority confirmation."
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (n *RaftNode) handleVote(w http.ResponseWriter, r *http.Request) {
	var req VoteRequest
	json.NewDecoder(r.Body).Decode(&req)
	n.mu.Lock()
	defer n.mu.Unlock()
	resp := VoteResponse{Term: n.term, VoteGranted: false}
	if req.Term > n.term {
		n.term = req.Term
		n.votedFor = ""
		n.state = Follower
	}
	if req.Term == n.term && (n.votedFor == "" || n.votedFor == req.CandidateId) {
		lastLogIndex := len(n.log) - 1
		lastLogTerm := 0
		if lastLogIndex >= 0 {
			lastLogTerm = n.log[lastLogIndex].Term
		}
		if req.LastLogTerm > lastLogTerm || (req.LastLogTerm == lastLogTerm && req.LastLogIndex >= lastLogIndex) {
			resp.VoteGranted = true
			n.votedFor = req.CandidateId
			select {
			case n.voteCh <- true:
			default:
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (n *RaftNode) handleAppend(w http.ResponseWriter, r *http.Request) {
	var req AppendRequest
	json.NewDecoder(r.Body).Decode(&req)
	n.mu.Lock()
	defer n.mu.Unlock()
	resp := AppendResponse{Term: n.term, Success: false}
	if req.Term > n.term {
		n.term = req.Term
		n.votedFor = ""
		n.state = Follower
	}
	if req.Term == n.term {
		n.state = Follower
		select {
		case n.appendCh <- true:
		default:
		}
		if req.PrevLogIndex == -1 || (req.PrevLogIndex < len(n.log) && n.log[req.PrevLogIndex].Term == req.PrevLogTerm) {
			resp.Success = true
			if len(req.Entries) > 0 {
				n.log = append(n.log[:req.PrevLogIndex+1], req.Entries...)
			}
			if req.LeaderCommit > n.commitIndex {
				n.commitIndex = min(req.LeaderCommit, len(n.log)-1)
			}
		}
	}
	resp.Term = n.term
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (n *RaftNode) handleStatus(w http.ResponseWriter, r *http.Request) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	status := map[string]interface{}{
		"id":           n.id,
		"state":        n.state.String(),
		"term":         n.term,
		"log_length":   len(n.log),
		"commit_index": n.commitIndex,
		"last_applied": n.lastApplied,
		"voted_for":    n.votedFor,
		"peers":        n.peers,
		"nextIndex":    n.nextIndex,
		"matchIndex":   n.matchIndex,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// Network functions
func (n *RaftNode) sendVoteRequest(peer string, req VoteRequest) (*VoteResponse, error) {
	data, _ := json.Marshal(req)
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://%s/vote", peer), "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var voteResp VoteResponse
	json.NewDecoder(resp.Body).Decode(&voteResp)
	return &voteResp, nil
}

func (n *RaftNode) sendAppendRequest(peer string, req AppendRequest) (*AppendResponse, error) {
	data, _ := json.Marshal(req)
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://%s/append", peer), "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var appendResp AppendResponse
	json.NewDecoder(resp.Body).Decode(&appendResp)
	return &appendResp, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run main.go <node_id> <port> <db_path> [peer1:port1] [peer2:port2] ...")
		os.Exit(1)
	}
	rand.Seed(time.Now().UnixNano())

	nodeId := os.Args[1]
	port, _ := strconv.Atoi(os.Args[2])
	dbPath := os.Args[3]
	peers := os.Args[4:]

	os.Remove(dbPath)

	node, err := NewRaftNode(nodeId, peers, port, dbPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Starting node %s on port %d, database path: %s", nodeId, port, dbPath)
	log.Printf("Peer node: %v", peers)
	node.Start()
	select {}
}
