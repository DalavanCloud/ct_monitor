package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/lib/pq"
	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/x509"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

type config struct {
	// Common configuration parameters shared by all processors.
	ConnInfo string
	ConnOpen int
	ConnIdle int
	ConnLife duration
	Interval duration
	Batch int
	Concurrent int
	// Processor-specific config.
	Chunk int
	HTTPTimeout duration
}

type CTLogDetails struct {
	id int
	url string
	tree_size sql.NullInt64
	latest_sth_timestamp time.Time
	latest_update time.Time
	start_time time.Time
}

type NewLogEntry struct {
	ct_log_id int
	entry_id int64
	entry_timestamp time.Time
	cert *x509.Certificate
	sha256_cert [sha256.Size]byte
	sha256_issuer [sha256.Size]byte
	issuer_verified bool
}

type Work struct {
	c *config
	db *sql.DB
	http_client http.Client
	get_log_list_statement *sql.Stmt
	create_getsth_temp_table_statement *sql.Stmt
	getsth_update_statement *sql.Stmt
	import_chain_cert_statement *sql.Stmt
	create_newentries_temp_table_statement *sql.Stmt
	newentries_update_statement *sql.Stmt
	wg_closeWorkers sync.WaitGroup
	chan_closeWorkers chan bool
	chan_newEntries chan NewLogEntry
	wg_batchCompletion sync.WaitGroup
	mutex_batchCompletion *sync.Mutex
}

type WorkItem struct {
	ct_log_id int
	ct_log_url string
	tree_size int64
	batch_size int64
	chunk_size sql.NullInt64
	start_entry_id int64
	start_time time.Time
}


// tomlConfig.DefineCustomFlags() and tomlConfig.PrintCustomFlags()
// Specify command-line flags that are specific to this processor.
func (c *config) DefineCustomFlags() {
	flag.IntVar(&c.Chunk, "chunk", c.Chunk, "Maximum number of entries processed per log per batch")
	flag.DurationVar(&c.HTTPTimeout.Duration, "httptimeout", c.HTTPTimeout.Duration, "HTTP timeout")
}
func (c *config) PrintCustomFlags() string {
	return fmt.Sprintf("chunk:%d httptimeout:%s", c.Chunk, c.HTTPTimeout.Duration)
}


// Work.Init()
// One-time initialization.
func (w *Work) Init(c *config) {
	w.c = c
	w.http_client = http.Client{Timeout: c.HTTPTimeout.Duration}

	if c.Chunk <= 0 {
		panic(fmt.Errorf("'chunk' must be a positive integer"))
	}

	var err error

	w.get_log_list_statement, err = w.db.Prepare(`
SELECT ctl.ID, ctl.URL, ctl.TREE_SIZE, coalesce(ctl.LATEST_STH_TIMESTAMP, 'epoch'::date)
	FROM ct_log ctl
	WHERE ctl.IS_ACTIVE
`)
	checkErr(err)

	w.create_getsth_temp_table_statement, err = w.db.Prepare(`
CREATE TEMP TABLE getsth_update_temp (
	CT_LOG_ID integer,
	TREE_SIZE bigint,
	LATEST_STH_TIMESTAMP timestamp,
	LATEST_UPDATE timestamp
) ON COMMIT DROP
`)
	checkErr(err)

	w.getsth_update_statement, err = w.db.Prepare("SELECT getsth_update()")
	checkErr(err)

	w.import_chain_cert_statement, err = w.db.Prepare("SELECT import_chain_cert($1,$2)")
	checkErr(err)

	w.create_newentries_temp_table_statement, err = w.db.Prepare(`
CREATE TEMP TABLE newentries_temp (
	CT_LOG_ID integer,
	ENTRY_ID bigint,
	ENTRY_TIMESTAMP timestamp,
	DER_X509 bytea,
    SHA256_X509 bytea,
	CERTIFICATE_ID bigint,
	ISSUER_CA_ID integer,
	SUBJECT_CA_ID integer,
	LINTING_APPLIES bool,
	NEW_CERT_COUNT integer			DEFAULT 1,
	NEW_AND_CAN_ISSUE_CERTS bool	DEFAULT 'f',
	IS_NEW_CA bool					DEFAULT 'f'
) ON COMMIT DROP
`)

	w.newentries_update_statement, err = w.db.Prepare("SELECT process_new_entries()")
	checkErr(err)

	// Create a channel to control terminating the worker goroutines, and another channel through which we'll send each new log entry to the newEntryWriter goroutine.
	w.chan_closeWorkers = make(chan bool)
	w.chan_newEntries = make(chan NewLogEntry, 1048576)

	// Spin up a goroutine that will regularly poll each log's /ct/v1/get-sth API, then verify each STH's signature and update ct_log{TREE_SIZE, LATEST_STH_TIMESTAMP, LATEST_UPDATE}.
	w.wg_closeWorkers.Add(1)
	go func() {
		defer func() {
			w.wg_closeWorkers.Done()
		}()
		w.sthMonitor()
	}()

	// Spin up the newEntryWriter goroutine, which will consume from the chan_newEntries channel and regularly bulk-import new log entries.
	w.wg_closeWorkers.Add(1)
	go func() {
		defer func() {
			w.wg_closeWorkers.Done()
		}()
		w.newEntryWriter()
	}()

	w.mutex_batchCompletion = &sync.Mutex{}
}

func (w *Work) logErr(ctld *CTLogDetails, status string, outcome string) {
	log.Printf("%v,%v,\"%s\",\"%s\",\"%s\"\n", time.Now().UTC(), time.Now().UTC().Sub(ctld.start_time), ctld.url, status, outcome)
}

func (w *Work) logSthMonitor(status string, outcome string) {
	log.Printf("%v,,\"[sthMonitor]\",\"%s\",\"%s\"\n", time.Now().UTC(), status, outcome)
}

func (w *Work) getSth(ctld *CTLogDetails) bool {
	ctld.start_time = time.Now().UTC()

	// Call this log's /ct/v1/get-sth API.
	http_req, err := http.NewRequest(http.MethodGet, ctld.url + "/ct/v1/get-sth", nil)
	if err != nil {
		w.logErr(ctld, "FAILED", fmt.Sprintf("http.NewRequest => %v", err))
		return false
	}
	http_resp, err := w.http_client.Do(http_req)
	if err != nil {
		w.logErr(ctld, "FAILED", fmt.Sprintf("http_client.Do => %v", err))
		return false
	}
	defer http_resp.Body.Close()

	// Read the HTTP response body.
	resp_body, err := ioutil.ReadAll(http_resp.Body)
	if err != nil {
		w.logErr(ctld, "FAILED", fmt.Sprintf("ioutil.ReadAll => %v", err))
		return false
	}

	// Parse the JSON in the HTTP response body.
	var get_sth ct.GetSTHResponse
	if http_resp.StatusCode == http.StatusOK {
		if err = json.Unmarshal(resp_body, &get_sth); err != nil {
			w.logErr(ctld, "FAILED", fmt.Sprintf("json.Unmarshal => %v", err))
			return false
		}
	} else {
		w.logErr(ctld, "FAILED", fmt.Sprintf("HTTP %d", http_resp.StatusCode))
		return false
	}

	// Copy the updated get-sth details.
	this_sth_timestamp := time.Unix(0, int64(get_sth.Timestamp) * int64(time.Millisecond)).UTC()
	if this_sth_timestamp.After(ctld.latest_sth_timestamp) {
		ctld.tree_size.Int64 = int64(get_sth.TreeSize)
		ctld.tree_size.Valid = true
		ctld.latest_sth_timestamp = this_sth_timestamp
	}
	ctld.latest_update = time.Now().UTC()

	// TODO: Verify STH signature.
    // TODO: Take some action if the STH signature is invalid.

	return true
}

func (w *Work) sthMonitor() {
	keep_looping := true
	for keep_looping {
		var tx *sql.Tx
		var tx_create_getsth_temp_table_statement *sql.Stmt
		var tx_getsth_update_statement *sql.Stmt
		var tx_copy_item_statement *sql.Stmt
		i := 0
		chan_getSTH := make(chan CTLogDetails)

		// List all of the logs that we're currently monitoring.
		rows, err := w.get_log_list_statement.Query()
		if err != nil {
			goto next
		}
		defer rows.Close()

		// Loop through the logs.
		for rows.Next() {
			// Get the details of one log.
			var ctld CTLogDetails
			err = rows.Scan(&ctld.id, &ctld.url, &ctld.tree_size, &ctld.latest_sth_timestamp)
			if err != nil {
				w.logSthMonitor("ERROR", err.Error())
			} else {
				i++
			}

			// Launch a goroutine to GET /ct/v1/get-sth for this log.
			go func() {
				if !w.getSth(&ctld) {
					ctld.id = -1
				}
				chan_getSTH <- ctld
			}()
		}

		// Start a transaction.
		tx, err = w.db.Begin()
		if err != nil {
			goto next
		}
		defer tx.Rollback()

		// Prepare some statements for this transaction.
		tx_create_getsth_temp_table_statement = tx.Stmt(w.create_getsth_temp_table_statement)
		defer tx_create_getsth_temp_table_statement.Close()
		tx_getsth_update_statement = tx.Stmt(w.getsth_update_statement)
		defer tx_getsth_update_statement.Close()

		// Create the temporary "getsth_update_temp" table.
		_, err = tx_create_getsth_temp_table_statement.Exec()
		if err != nil {
			goto next
		}

		// Prepare the COPY statement.
		tx_copy_item_statement, err = tx.Prepare(pq.CopyIn("getsth_update_temp", "ct_log_id", "tree_size", "latest_sth_timestamp", "latest_update"))
		if err != nil {
			goto next
		}

		// Add rows to the COPY statement.
		for ; i > 0; i-- {
			ctld := <-chan_getSTH
			if ctld.id != -1 {
				_, err = tx_copy_item_statement.Exec(ctld.id, ctld.tree_size, ctld.latest_sth_timestamp, ctld.latest_update)
				if err != nil {
					w.logSthMonitor("ERROR", err.Error())
				}
			}
		}

		// Execute the COPY statement.
		_, err = tx_copy_item_statement.Exec()
		if err != nil {
			goto next
		}

		// Process the COPYed data.
		_, err = tx_getsth_update_statement.Exec()
		if err != nil {
			goto next
		}

		// Commit the transaction.  This will drop the temporary table.
		err = tx.Commit()

	next:
		if err != nil {
			w.logSthMonitor("ERROR", err.Error())
		}
		// Wait for the next 1-minute interval, or exit the loop if required.
		select {
			case <-w.chan_closeWorkers:
				keep_looping = false

			case <-time.After(time.Now().UTC().Truncate(1 * time.Minute).Add(1 * time.Minute).Sub(time.Now().UTC())):
		}
	}
}

func (w *Work) logNewEntryWriter(status string, outcome string, start_time time.Time) {
	log.Printf("%v,%v,\"[newEntryWriter]\",\"%s\",\"%s\"\n", time.Now().UTC(), time.Now().Sub(start_time), status, outcome)
}

func (w *Work) newEntryWriter() {
	keep_looping := true
	acks_to_send := 0
	const MAX_CERTS_TO_COPY = 256
	const PROCESSING_FREQUENCY = 5 * time.Second
	var certs_to_copy []NewLogEntry
	sha256_issuer_cache := make(map[[sha256.Size]byte]sql.NullInt64)
	var err error
	var start_time time.Time
	var len_certs_to_copy int
	var len_queue int

	for keep_looping {
		var tx *sql.Tx
		var tx_create_newentries_temp_table_statement *sql.Stmt
		var tx_copy_item_statement *sql.Stmt
		var tx_newentries_update_statement *sql.Stmt

		if certs_to_copy == nil {
			certs_to_copy = make([]NewLogEntry, 0, MAX_CERTS_TO_COPY)
		}

		select {
			case <-w.chan_closeWorkers:
				keep_looping = false

			case new_log_entry := <-w.chan_newEntries:
				// If we found a valid issuer cert for this cert, let's see if we've already cached its crt.sh CA ID.
				var issuer_ca_id sql.NullInt64
				if new_log_entry.issuer_verified {
					issuer_ca_id = sha256_issuer_cache[new_log_entry.sha256_issuer]
				}

				if new_log_entry.ct_log_id == -999 {		// End of batch notification.
					acks_to_send++
					if len(certs_to_copy) > 0 {
						goto copy_certs
					} else {
						goto ack
					}
				} else if new_log_entry.ct_log_id == -1 {	// CA certificate from the entry's chain.
					// Let's see if we've already cached this CA certificate's crt.sh CA ID.
					ca_id := sha256_issuer_cache[new_log_entry.sha256_cert]
					if !ca_id.Valid {		// It's not already cached, so import and cache it now.
						if err = w.import_chain_cert_statement.QueryRow(new_log_entry.cert.Raw, issuer_ca_id).Scan(&ca_id); err == nil {
							sha256_issuer_cache[new_log_entry.sha256_cert] = ca_id
						}
					}
				} else {									// Certificate or precertificate entry.
					// Queue this log entry to be COPYed.
					certs_to_copy = append(certs_to_copy, new_log_entry)

					// If the queue is full, process the entries now.
					if len(certs_to_copy) >= MAX_CERTS_TO_COPY {
						goto copy_certs
					}
				}

			case <-time.After(time.Now().UTC().Truncate(PROCESSING_FREQUENCY).Add(PROCESSING_FREQUENCY).Sub(time.Now().UTC())):
				// If any entries are queued, process them now.
				if len(certs_to_copy) > 0 {
					goto copy_certs
				}
		}
		continue

	copy_certs:
		len_certs_to_copy = len(certs_to_copy)
		len_queue = len(w.chan_newEntries) + len(certs_to_copy)
		start_time = time.Now().UTC()

		// Start a transaction.
		if tx, err = w.db.Begin(); err != nil {
			goto next
		}
		defer tx.Rollback()

		// Prepare some statements for this transaction.
		tx_create_newentries_temp_table_statement = tx.Stmt(w.create_newentries_temp_table_statement)
		defer tx_create_newentries_temp_table_statement.Close()
		tx_newentries_update_statement = tx.Stmt(w.newentries_update_statement)
		defer tx_newentries_update_statement.Close()

		// Create the temporary "newentries_temp" table.
		if _, err = tx_create_newentries_temp_table_statement.Exec(); err != nil {
			goto next
		}

		// Prepare the COPY statement.
		if tx_copy_item_statement, err = tx.Prepare(pq.CopyIn("newentries_temp", "ct_log_id", "entry_id", "entry_timestamp", "der_x509", "sha256_x509", "issuer_ca_id")); err != nil {
			goto next
		}

		// Add rows to the COPY statement.
		for _, entry := range certs_to_copy {
			var issuer_ca_id sql.NullInt64
			if entry.issuer_verified {
				issuer_ca_id = sha256_issuer_cache[entry.sha256_issuer]
			}

			if _, err = tx_copy_item_statement.Exec(entry.ct_log_id, entry.entry_id, entry.entry_timestamp, entry.cert.Raw, entry.sha256_cert[:], issuer_ca_id); err != nil {
				goto next
			}
		}

		// Execute the COPY statement.
		if _, err = tx_copy_item_statement.Exec(); err != nil {
			goto next
		}

		// Process the COPYed data.
		if _, err = tx_newentries_update_statement.Exec(); err != nil {
			goto next
		}

		// Commit the transaction.  This will drop the temporary table.
		err = tx.Commit()

		w.logNewEntryWriter("INFO", fmt.Sprintf("Processed %d (of %d)", len_certs_to_copy, len_queue), start_time)

	next:
		if err != nil {
			w.logNewEntryWriter("ERROR", err.Error(), start_time)
			// TODO: Avoid hanging when an error occurs.
		}

		// Empty the queue, now that we've processed it.
		certs_to_copy = nil

	ack:
		for acks_to_send > 0 {
			w.wg_batchCompletion.Done()
			acks_to_send--
		}
	}

	// TODO: Build each log's tree, in order to check that the hash produced matches the STH.
}

// Work.Begin()
// Per-batch initialization.
func (w *Work) Begin(db *sql.DB) {
	w.db = db
}

// Work.End
// Per-batch post-processing.
func (w *Work) End() {
}

// Work.Exit
// One-time program exit code.
func (w *Work) Exit() {
	w.chan_closeWorkers <- true
	w.chan_closeWorkers <- true
	w.wg_closeWorkers.Wait()

	w.get_log_list_statement.Close()
	w.create_getsth_temp_table_statement.Close()
	w.getsth_update_statement.Close()
	w.import_chain_cert_statement.Close()
	w.create_newentries_temp_table_statement.Close()
	w.newentries_update_statement.Close()
}

func (wi *WorkItem) logErr(url string, status string, outcome string) {
	log.Printf("%v,%v,\"%s\",\"%s\",\"%s\"\n", time.Now().UTC(), time.Now().UTC().Sub(wi.start_time), url, status, outcome)
}

// Work.Prepare()
// Prepare the driving SELECT query.
func (w *Work) SelectQuery(batch_size int) string {
	return fmt.Sprintf(`
SELECT ctl.ID, ctl.URL, coalesce(ctl.TREE_SIZE, 0), coalesce(ctl.BATCH_SIZE, 256), ctl.CHUNK_SIZE, latest.ENTRY_ID + 1
	FROM ct_log ctl
			LEFT JOIN LATERAL (
				SELECT coalesce(max(ctle.ENTRY_ID), -1) ENTRY_ID
					FROM ct_log_entry ctle
					WHERE ctle.CT_LOG_ID = ctl.ID
			) latest ON TRUE
	WHERE ctl.IS_ACTIVE
		AND latest.ENTRY_ID < (ctl.TREE_SIZE - 1)
	ORDER BY ctl.LATEST_UPDATE
	LIMIT %d
`, batch_size)
}

// WorkItem.Parse()
// Parse one SELECTed row to configure one work item.
func (wi *WorkItem) Parse(rs *sql.Rows) error {
	return rs.Scan(&wi.ct_log_id, &wi.ct_log_url, &wi.tree_size, &wi.batch_size, &wi.chunk_size, &wi.start_entry_id)
}

// WorkItem.Perform()
// Do the work for one item.
func (wi *WorkItem) Perform(db *sql.DB, w *Work) {
	num_entries := w.c.Chunk
	if wi.chunk_size.Valid {
		num_entries = int(wi.chunk_size.Int64)
	}
	start_start := wi.start_entry_id
	start := start_start

	for (start < wi.tree_size) && (num_entries > 0) {
		wi.start_time = time.Now().UTC()

		// Determine the start and end parameters to pass to /ct/v1/get-entries.
		end := start + wi.batch_size - 1
		if end >= wi.tree_size {
			end = wi.tree_size - 1
		}
		if (end - start + 1) > int64(num_entries) {
			end = start + int64(num_entries) - 1
		}

		// Call this log's /ct/v1/get-entries API.
		get_entries_url := fmt.Sprintf("%s/ct/v1/get-entries?start=%d&end=%d", wi.ct_log_url, start, end)

		http_req, err := http.NewRequest(http.MethodGet, get_entries_url, nil)
		if err != nil {
			wi.logErr(get_entries_url, "FAILED", fmt.Sprintf("http.NewRequest => %v", err))
			return
		}
		http_resp, err := w.http_client.Do(http_req)
		if err != nil {
			wi.logErr(get_entries_url, "FAILED", fmt.Sprintf("http_client.Do => %v", err))
			return
		}
		defer http_resp.Body.Close()

		// Read the HTTP response body.
		resp_body, err := ioutil.ReadAll(http_resp.Body)
		if err != nil {
			wi.logErr(get_entries_url, "FAILED", fmt.Sprintf("ioutil.ReadAll => %v", err))
			return
		}

		// Parse the JSON in the HTTP response body.
		var get_entries ct.GetEntriesResponse
		if http_resp.StatusCode == http.StatusOK {
			if err = json.Unmarshal(resp_body, &get_entries); err != nil {
				wi.logErr(get_entries_url, "FAILED", fmt.Sprintf("json.Unmarshal => %v", err))
				return
			}
		} else {
			wi.logErr(get_entries_url, "FAILED", fmt.Sprintf("HTTP %d", http_resp.StatusCode))
			return
		}

		// Loop through the entries.
		for _, entry := range get_entries.Entries {
			// Construct log entry structure.
			log_entry, err := ct.LogEntryFromLeaf(start, &entry)
			if x509.IsFatal(err) {
				wi.logErr(get_entries_url, "ERROR", fmt.Sprintf("Entry #%d: %v", start, err))
				return
			}
			new_log_entry := NewLogEntry{
				ct_log_id: -1,
			}

			var issuer_cert *x509.Certificate
			for i := len(log_entry.Chain) - 1; i >= 0; i-- {
				new_log_entry.issuer_verified = false
				new_log_entry.cert, err = x509.ParseCertificate(log_entry.Chain[i].Data)
				if err != nil {
					wi.logErr(get_entries_url, "ERROR", fmt.Sprintf("Entry #%d: %v", log_entry.Index, err))
				} else if issuer_cert != nil {
					if new_log_entry.cert.CheckSignatureFrom(issuer_cert) == nil {
						// Signature is valid, so pass the parent certificate's SHA-256 hash.
						new_log_entry.sha256_issuer = sha256.Sum256(issuer_cert.Raw)
						new_log_entry.issuer_verified = true
					}
				}

				new_log_entry.sha256_cert = sha256.Sum256(new_log_entry.cert.Raw)

				// Send this CA certificate to the newEntryWriter.
				w.chan_newEntries <- new_log_entry

				issuer_cert = new_log_entry.cert
			}

			// Process the certificate or precertificate entry.
			new_log_entry.ct_log_id = wi.ct_log_id
			new_log_entry.issuer_verified = false
			new_log_entry.entry_id = log_entry.Index
			new_log_entry.entry_timestamp = ct.TimestampToTime(log_entry.Leaf.TimestampedEntry.Timestamp).UTC()
			if log_entry.X509Cert != nil {
				new_log_entry.cert = log_entry.X509Cert
			} else if log_entry.Precert != nil {
				new_log_entry.cert, err = x509.ParseCertificate(log_entry.Precert.Submitted.Data)
				if err != nil {
					wi.logErr(get_entries_url, "WARNING", fmt.Sprintf("Entry #%d: x509.ParseCertificate(precertificate) => %v", log_entry.Index, err))
				}
			} else {
				wi.logErr(get_entries_url, "ERROR", fmt.Sprintf("Entry #%d: Unsupported entry type", log_entry.Index))
				return
			}

			new_log_entry.sha256_cert = sha256.Sum256(new_log_entry.cert.Raw)

			// Verify the certificate or precertificate's signature, if possible.
			if (new_log_entry.cert != nil) && (issuer_cert != nil) {
				if new_log_entry.cert.CheckSignatureFrom(issuer_cert) == nil {
					// Signature is valid, so pass the parent certificate's SHA-256 hash.
					new_log_entry.sha256_issuer = sha256.Sum256(issuer_cert.Raw)
					new_log_entry.issuer_verified = true
				}
			}

			// TODO: Verify SCT signature.

			// Send this certificate or precertificate entry to the newEntryWriter.
			w.chan_newEntries <- new_log_entry

			// Move to the next entry.
			start++
			num_entries--
		}
	}

	// Send a dummy entry to the newEntryWriter to signal that we've finished sending.
	w.mutex_batchCompletion.Lock()
	w.wg_batchCompletion.Add(1)
	finished_log_entry := NewLogEntry{
		ct_log_id: -999,
	}
	w.chan_newEntries <- finished_log_entry
	// Wait for an ack from the newEntryWriter.
	w.wg_batchCompletion.Wait()
	w.mutex_batchCompletion.Unlock()

	wi.logErr(wi.ct_log_url, "INFO", fmt.Sprintf("Processed %d entries", (start - start_start)))
}

// Work.UpdateStatement()
// Prepare the UPDATE statement to be run after processing each work item (chunk).
func (w *Work) UpdateStatement() string {
	return ""
}

// WorkItem.Update()
// Update the DB with the results of the work for this item (chunk).
func (wi *WorkItem) Update(update_statement *sql.Stmt) (sql.Result, error) {
	return nil, nil
}
