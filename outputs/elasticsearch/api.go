package elasticsearch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/elastic/libbeat/logp"
)

type Elasticsearch struct {
	MaxRetries     int
	connectionPool ConnectionPool
	client         *http.Client
}

type QueryResult struct {
	Ok      bool            `json:"ok"`
	Index   string          `json:"_index"`
	Type    string          `json:"_type"`
	Id      string          `json:"_id"`
	Source  json.RawMessage `json:"_source"`
	Version int             `json:"_version"`
	Found   bool            `json:"found"`
	Exists  bool            `json:"exists"`
	Created bool            `json:"created"`
	Matches []string        `json:"matches"`
}

type SearchResults struct {
	Took   int                        `json:"took"`
	Shards json.RawMessage            `json:"_shards"`
	Hits   Hits                       `json:"hits"`
	Aggs   map[string]json.RawMessage `json:"aggregations"`
}

type Hits struct {
	Total int
	Hits  []json.RawMessage `json:"hits"`
}

func (r QueryResult) String() string {
	out, err := json.Marshal(r)
	if err != nil {
		return "ERROR"
	}
	return string(out)
}

const (
	default_max_retries = 3
)

// Create a connection to Elasticsearch
func NewElasticsearch(urls []string, username string, password string) *Elasticsearch {

	var connection_pool ConnectionPool
	connection_pool.SetConnections(urls, username, password)

	es := Elasticsearch{
		connectionPool: connection_pool,
		client:         &http.Client{},
		MaxRetries:     default_max_retries,
	}
	return &es
}

// Encode parameters in url
func UrlEncode(params map[string]string) string {
	var values url.Values = url.Values{}

	for key, val := range params {
		values.Add(key, string(val))
	}
	return values.Encode()
}

// Create path out of index, doc_type and id that is used for querying Elasticsearch
func MakePath(index string, doc_type string, id string) (string, error) {

	var path string
	if len(doc_type) > 0 {
		if len(id) > 0 {
			path = fmt.Sprintf("/%s/%s/%s", index, doc_type, id)
		} else {
			path = fmt.Sprintf("/%s/%s", index, doc_type)
		}
	} else {
		if len(id) > 0 {
			if len(index) > 0 {
				path = fmt.Sprintf("/%s/%s", index, id)
			} else {
				path = fmt.Sprintf("/%s", id)
			}
		} else {
			path = fmt.Sprintf("/%s", index)
		}
	}
	return path, nil
}

func ReadQueryResult(obj []byte) (*QueryResult, error) {

	var result QueryResult
	err := json.Unmarshal(obj, &result)
	if err != nil {
		return nil, err
	}
	return &result, err
}

func ReadSearchResult(obj []byte) (*SearchResults, error) {

	var result SearchResults
	err := json.Unmarshal(obj, &result)
	if err != nil {
		return nil, err
	}
	return &result, err
}

func (es *Elasticsearch) SetMaxRetries(max_retries int) {
	es.MaxRetries = max_retries
}

// Perform the actual request. If the operation was successful, mark it as live and return the response.
// If it fails, mark it as dead for a period of time.
func (es *Elasticsearch) PerformRequest(conn *Connection, req *http.Request) ([]byte, error) {

	req.Header.Add("Accept", "application/json")
	if conn.Username != "" || conn.Password != "" {
		req.SetBasicAuth(conn.Username, conn.Password)
	}

	resp, err := es.client.Do(req)
	if err != nil {
		// request fails
		logp.Warn("Fail to send the request to Elasticsearch: %s", err)
		es.connectionPool.MarkDead(conn)
		return nil, err
	}

	if resp.StatusCode > 499 {
		// request fails
		es.connectionPool.MarkDead(conn)
		return nil, fmt.Errorf("%d response from Elasticsearch", resp.StatusCode)
	}

	defer resp.Body.Close()
	obj, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logp.Warn("Fail to read the response from Elasticsearch")
		es.connectionPool.MarkDead(conn)
		return nil, err
	}

	// request with success
	es.connectionPool.MarkLive(conn)

	return obj, nil

}

// Create an HTTP request and send it to Elasticsearch. The request is retransmitted max_retries
// before returning an error.
func (es *Elasticsearch) Request(method string, url string,
	params map[string]string, body interface{}) ([]byte, error) {

	for attempt := 0; attempt < es.MaxRetries; attempt++ {

		conn := es.connectionPool.GetConnection()
		logp.Debug("elasticsearch", "Use connection %s", conn.Url)

		url = conn.Url + url
		if len(params) > 0 {
			url = url + "?" + UrlEncode(params)
		}

		var obj []byte
		var err error
		if body != nil {
			obj, err = json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("Fail to JSON encode the body: %s", err)
			}
		} else {
			obj = nil
		}
		req, err := http.NewRequest(method, url, bytes.NewReader(obj))
		if err != nil {
			return nil, err
		}

		logp.Debug("elasticsearch", "Sending request to %s", url)

		resp, err := es.PerformRequest(conn, req)
		if err != nil {
			// retry
			continue
		}
		return resp, nil

	}

	logp.Warn("Request fails to be send after %d retries", es.MaxRetries)

	return nil, fmt.Errorf("Request fails to be sent after %d retries", es.MaxRetries)
}

// Index adds or updates a typed JSON document in a specified index, making it
// searchable. In case id is empty, a new id is created over a HTTP POST request.
// Otherwise, a HTTP PUT request is issued.
// Implements: http://www.elastic.co/guide/en/elasticsearch/reference/current/docs-index_.html
func (es *Elasticsearch) Index(index string, doc_type string, id string,
	params map[string]string, body interface{}) (*QueryResult, error) {

	var method string

	path, err := MakePath(index, doc_type, id)
	if err != nil {
		return nil, err
	}
	if len(id) == 0 {
		method = "POST"
	} else {
		method = "PUT"
	}
	resp, err := es.Request(method, path, params, body)
	if err != nil {
		return nil, err
	}
	return ReadQueryResult(resp)
}

// Refresh an index. Call this after doing inserts or creating/deleting
// indexes in unit tests.
func (es *Elasticsearch) Refresh(index string) (*QueryResult, error) {
	path, err := MakePath(index, "", "_refresh")
	if err != nil {
		return nil, err
	}
	resp, err := es.Request("POST", path, nil, nil)
	if err != nil {
		return nil, err
	}

	return ReadQueryResult(resp)
}

// Instantiate an index
func (es *Elasticsearch) CreateIndex(index string) (*QueryResult, error) {

	path, err := MakePath(index, "", "")
	if err != nil {
		return nil, err
	}

	resp, err := es.Request("PUT", path, nil, nil)
	if err != nil {
		return nil, err
	}

	return ReadQueryResult(resp)
}

// Deletes a typed JSON document from a specific index based on its id.
// Implements: http://www.elastic.co/guide/en/elasticsearch/reference/current/docs-delete.html
func (es *Elasticsearch) Delete(index string, doc_type string, id string, params map[string]string) (*QueryResult, error) {

	path, err := MakePath(index, doc_type, id)
	if err != nil {
		return nil, err
	}

	resp, err := es.Request("DELETE", path, params, nil)
	if err != nil {
		return nil, err
	}

	return ReadQueryResult(resp)
}

// A search request can be executed purely using a URI by providing request parameters.
// Implements: http://www.elastic.co/guide/en/elasticsearch/reference/current/search-uri-request.html
func (es *Elasticsearch) SearchUri(index string, doc_type string, params map[string]string) (*SearchResults, error) {

	path, err := MakePath(index, doc_type, "_search")
	if err != nil {
		return nil, err
	}

	resp, err := es.Request("GET", path, params, nil)
	if err != nil {
		return nil, err
	}
	return ReadSearchResult(resp)
}
