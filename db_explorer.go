package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	_ "github.com/go-sql-driver/mysql"
)

const (
	ErrMethodNotAllowed = `{"error": "method not allowed"}`
	ErrUnknownTable     = `{"error": "unknown table"}`
	ErrInvalidID        = `{"error": "invalid id"}`
	ErrRecordNotFound   = `{"error": "record not found"}`
	ErrNotFound         = `{"error": "not found"}`
	ErrInvalidJSON      = `{"error": "invalid json"}`
	ErrInternalServer   = `{"error": "internal server error"}`
	ErrNoValidFields    = `{"error": "no valid fields provided"}`
	ErrNoValidUpdate    = `{"error": "no valid fields to update"}`
)

var ErrNoPrimaryKey = errors.New("no primary key defined")

type Column struct {
	Name       string
	Type       string
	IsNullable bool
	IsPrimary  bool
}

type Table struct {
	Name       string
	Columns    []Column
	PrimaryKey string
}

type DBExplorer struct {
	tables  map[string]Table
	db      *sql.DB
	mu      sync.RWMutex
	colPool sync.Pool
}

func NewDBExplorer(db *sql.DB) (http.Handler, error) {
	explorer := &DBExplorer{
		tables: make(map[string]Table),
		db:     db,
		colPool: sync.Pool{
			New: func() interface{} {
				return struct {
					cols    []interface{}
					colPtrs []interface{}
				}{}
			},
		},
	}

	tableRows, err := db.Query("SHOW TABLES;")
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := tableRows.Close(); cerr != nil {
			log.Printf("Error closing tableRows: %v\n", cerr)
		}
	}()

	var tableName string
	for tableRows.Next() {
		if err := tableRows.Scan(&tableName); err != nil {
			return nil, err
		}
		explorer.tables[tableName] = Table{
			Name:    tableName,
			Columns: []Column{},
		}
	}
	if err := tableRows.Err(); err != nil {
		return nil, err
	}

	for name, table := range explorer.tables {
		colQuery := fmt.Sprintf("SHOW FULL COLUMNS FROM `%s`;", name)
		colRows, err := db.Query(colQuery)
		if err != nil {
			return nil, err
		}

		var columnName, colType, colNull, colKey, colExtra, colPrivileges, colComment string
		var colCollation, colDefault sql.NullString

		for colRows.Next() {
			if err := colRows.Scan(
				&columnName,
				&colType,
				&colCollation,
				&colNull,
				&colKey,
				&colDefault,
				&colExtra,
				&colPrivileges,
				&colComment,
			); err != nil {
				return nil, fmt.Errorf("error scanning column metadata: %w", err)
			}

			column := Column{
				Name:       columnName,
				Type:       colType,
				IsNullable: colNull == "YES",
				IsPrimary:  colKey == "PRI",
			}

			if column.IsPrimary {
				table.PrimaryKey = columnName
			}

			table.Columns = append(table.Columns, column)
		}

		if err := colRows.Err(); err != nil {
			return nil, fmt.Errorf("error iterating column metadata rows: %w", err)
		}

		if err := colRows.Close(); err != nil {
			return nil, fmt.Errorf("failed to close tableRows: %w", err)
		}
		explorer.tables[name] = table
	}

	return explorer, nil
}

func (e *DBExplorer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	e.mu.RLock()
	defer e.mu.RUnlock()

	switch len(parts) {
	case 0, 1:
		if len(parts) == 0 || parts[0] == "" {
			if r.Method == http.MethodGet {
				e.handleListTables(w)
				return
			}
			http.Error(w, ErrMethodNotAllowed, http.StatusMethodNotAllowed)
			return
		}
		tableName := parts[0]
		table, exists := e.tables[tableName]
		if !exists {
			http.Error(w, ErrUnknownTable, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			e.handleListRecords(w, r, table)
			return
		case http.MethodPut:
			e.handleCreateRecord(w, r, table)
			return
		default:
			http.Error(w, ErrMethodNotAllowed, http.StatusMethodNotAllowed)
			return
		}
	case 2:
		tableName := parts[0]
		id := parts[1]
		table, exists := e.tables[tableName]
		if !exists {
			http.Error(w, ErrUnknownTable, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			e.handleGetRecord(w, table, id)
			return
		case http.MethodPost:
			e.handleUpdateRecord(w, r, table, id)
			return
		case http.MethodDelete:
			e.handleDeleteRecord(w, table, id)
			return
		default:
			http.Error(w, ErrMethodNotAllowed, http.StatusMethodNotAllowed)
			return
		}
	default:
		http.Error(w, ErrNotFound, http.StatusNotFound)
		return
	}
}

func (e *DBExplorer) handleListTables(w http.ResponseWriter) {
	tables := make([]string, 0, len(e.tables))
	for name := range e.tables {
		tables = append(tables, name)
	}
	response := map[string]interface{}{
		"response": map[string]interface{}{
			"tables": tables,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func (e *DBExplorer) handleListRecords(w http.ResponseWriter, r *http.Request, table Table) {
	limit := 5
	offset := 0
	q := r.URL.Query()
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 {
		limit = l
	}
	if o, err := strconv.Atoi(q.Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	query := fmt.Sprintf("SELECT * FROM `%s` LIMIT ? OFFSET ?;", table.Name)
	rows, err := e.db.Query(query, limit, offset)
	if err != nil {
		log.Printf("Failed to execute query: %s, error: %v", query, err)
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			log.Printf("Failed to close rows: %v", cerr)
		}
	}()

	columns, err := rows.Columns()
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	var records []map[string]interface{}

	for rows.Next() {
		poolItem := e.colPool.Get().(struct {
			cols    []interface{}
			colPtrs []interface{}
		})
		poolItem.cols = make([]interface{}, len(columns))
		poolItem.colPtrs = make([]interface{}, len(columns))

		for i := range poolItem.cols {
			poolItem.colPtrs[i] = &poolItem.cols[i]
		}

		if err := rows.Scan(poolItem.colPtrs...); err != nil {
			http.Error(w, ErrInternalServer, http.StatusInternalServerError)
			return
		}

		record := make(map[string]interface{})
		for i, colName := range columns {
			val := poolItem.cols[i]
			if b, ok := val.([]byte); ok {
				record[colName] = string(b)
			} else {
				record[colName] = val
			}
		}
		records = append(records, record)
	}

	response := map[string]interface{}{
		"response": map[string]interface{}{
			"records": records,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func (e *DBExplorer) handleGetRecord(w http.ResponseWriter, table Table, id string) {
	pkValue, err := e.preparePKValue(table, id)
	if err != nil {
		http.Error(w, ErrInvalidID, http.StatusBadRequest)
		return
	}

	query := fmt.Sprintf("SELECT * FROM `%s` WHERE `%s` = ?;", table.Name, table.PrimaryKey)
	row := e.db.QueryRow(query, pkValue)

	columns, err := e.getTableColumns(table)
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	poolItem := e.colPool.Get().(struct {
		cols    []interface{}
		colPtrs []interface{}
	})
	poolItem.cols = make([]interface{}, len(columns))
	poolItem.colPtrs = make([]interface{}, len(columns))

	for i := range poolItem.cols {
		poolItem.colPtrs[i] = &poolItem.cols[i]
	}

	if err := row.Scan(poolItem.colPtrs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, ErrRecordNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	record := make(map[string]interface{})
	for i, colName := range columns {
		val := poolItem.cols[i]
		if b, ok := val.([]byte); ok {
			record[colName] = string(b)
		} else {
			record[colName] = val
		}
	}

	poolItem.cols = nil
	poolItem.colPtrs = nil
	e.colPool.Put(poolItem)

	response := map[string]interface{}{
		"response": map[string]interface{}{
			"record": record,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func getDefaultForColumn(col Column) interface{} {
	switch {
	case strings.HasPrefix(col.Type, "varchar"), strings.HasPrefix(col.Type, "text"):
		return ""
	case strings.HasPrefix(col.Type, "int"):
		return 0
	case strings.HasPrefix(col.Type, "float"), strings.HasPrefix(col.Type, "double"), strings.HasPrefix(col.Type, "decimal"):
		return 0.0
	default:
		return nil
	}
}

func (e *DBExplorer) handleCreateRecord(w http.ResponseWriter, r *http.Request, table Table) {
	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, ErrInvalidJSON, http.StatusBadRequest)
		return
	}

	var (
		columns      []string
		values       []interface{}
		placeholders []string
	)

	for _, col := range table.Columns {
		if col.Name == table.PrimaryKey {
			continue
		}
		if val, ok := data[col.Name]; ok {
			valid, parsedVal := e.validateAndParseValue(col, val)
			if !valid {
				msg := fmt.Sprintf(`{"error": "field %s have invalid type"}`, col.Name)
				log.Printf("Validation error: %s\n", msg)
				http.Error(w, msg, http.StatusBadRequest)
				return
			}
			columns = append(columns, fmt.Sprintf("`%s`", col.Name))
			values = append(values, parsedVal)
			placeholders = append(placeholders, "?")
		} else if !col.IsNullable {
			defaultValue := getDefaultForColumn(col)
			columns = append(columns, fmt.Sprintf("`%s`", col.Name))
			values = append(values, defaultValue)
			placeholders = append(placeholders, "?")
		}
	}

	if len(columns) == 0 {
		http.Error(w, ErrNoValidFields, http.StatusBadRequest)
		return
	}

	query := fmt.Sprintf("INSERT INTO `%s` (%s) VALUES (%s);", table.Name, strings.Join(columns, ", "), strings.Join(placeholders, ", "))

	result, err := e.db.Exec(query, values...)
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	insertID, err := result.LastInsertId()
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"response": map[string]interface{}{
			table.PrimaryKey: insertID,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func (e *DBExplorer) handleUpdateRecord(w http.ResponseWriter, r *http.Request, table Table, id string) {
	var data map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, ErrInvalidJSON, http.StatusBadRequest)
		return
	}

	pkValue, err := e.preparePKValue(table, id)
	if err != nil {
		http.Error(w, ErrInvalidID, http.StatusBadRequest)
		return
	}

	var setClauses []string
	var values []interface{}
	for _, col := range table.Columns {
		if col.Name == table.PrimaryKey {
			if _, ok := data[col.Name]; ok {
				http.Error(w, fmt.Sprintf(`{"error": "field %s have invalid type"}`, col.Name), http.StatusBadRequest)
				return
			}
			continue
		}
		if val, ok := data[col.Name]; ok {
			valid, parsedVal := e.validateAndParseValue(col, val)
			if !valid {
				msg := fmt.Sprintf(`{"error": "field %s have invalid type"}`, col.Name)
				http.Error(w, msg, http.StatusBadRequest)
				return
			}
			setClauses = append(setClauses, fmt.Sprintf("`%s` = ?", col.Name))
			values = append(values, parsedVal)
		}
	}

	if len(setClauses) == 0 {
		http.Error(w, ErrNoValidUpdate, http.StatusBadRequest)
		return
	}

	query := fmt.Sprintf("UPDATE `%s` SET %s WHERE `%s` = ?;", table.Name, strings.Join(setClauses, ", "), table.PrimaryKey)
	values = append(values, pkValue)

	result, err := e.db.Exec(query, values...)
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"response": map[string]interface{}{
			"updated": rowsAffected,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func (e *DBExplorer) handleDeleteRecord(w http.ResponseWriter, table Table, id string) {
	pkValue, err := e.preparePKValue(table, id)
	if err != nil {
		http.Error(w, ErrInvalidID, http.StatusBadRequest)
		return
	}

	query := fmt.Sprintf("DELETE FROM `%s` WHERE `%s` = ?;", table.Name, table.PrimaryKey)
	result, err := e.db.Exec(query, pkValue)
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"response": map[string]interface{}{
			"deleted": rowsAffected,
		},
	}
	e.writeJSON(w, http.StatusOK, response)
}

func (e *DBExplorer) preparePKValue(table Table, id string) (interface{}, error) {
	var pkValue interface{}
	var pkType string
	for _, col := range table.Columns {
		if col.IsPrimary {
			pkType = col.Type
			break
		}
	}
	if pkType == "" {
		return nil, ErrNoPrimaryKey
	}

	if strings.Contains(pkType, "int") {
		idInt, err := strconv.Atoi(id)
		if err != nil {
			return nil, err
		}
		pkValue = idInt
	} else {
		pkValue = id
	}
	return pkValue, nil
}

func (e *DBExplorer) validateAndParseValue(col Column, val interface{}) (bool, interface{}) {
	if val == nil {
		if col.IsNullable {
			return true, nil
		}
		return false, nil
	}

	switch {
	case strings.HasPrefix(col.Type, "int"):
		switch v := val.(type) {
		case float64:
			return true, int(v)
		case int:
			return true, v
		case string:
			i, err := strconv.Atoi(v)
			if err != nil {
				return false, nil
			}
			return true, i
		default:
			return false, nil
		}
	case strings.HasPrefix(col.Type, "float") || strings.HasPrefix(col.Type, "double") || strings.HasPrefix(col.Type, "decimal"):
		switch v := val.(type) {
		case float64:
			return true, v
		case int:
			return true, float64(v)
		case string:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return false, nil
			}
			return true, f
		default:
			return false, nil
		}
	case strings.HasPrefix(col.Type, "varchar") || strings.HasPrefix(col.Type, "text") || strings.HasPrefix(col.Type, "char"):
		switch v := val.(type) {
		case string:
			return true, v
		default:
			return false, nil
		}
	default:
		switch v := val.(type) {
		case string:
			return true, v
		default:
			return false, nil
		}
	}
}

func (e *DBExplorer) getTableColumns(table Table) ([]string, error) {
	if len(table.Columns) == 0 {
		return nil, fmt.Errorf("table %s has no columns", table.Name)
	}

	columns := make([]string, len(table.Columns))

	for i, col := range table.Columns {
		columns[i] = col.Name
	}

	return columns, nil
}

func (e *DBExplorer) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding response to JSON: %v\n", err)
		http.Error(w, ErrInternalServer, http.StatusInternalServerError)
		return
	}
}
