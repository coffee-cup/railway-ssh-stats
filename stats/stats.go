package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

const (
	RAILWAY_API_URL = "https://backboard.railway.com/graphql/v2"
)

type PublicStats struct {
	TotalUsers                int `json:"totalUsers"`
	TotalProjects             int `json:"totalProjects"`
	TotalServices             int `json:"totalServices"`
	TotalDeploymentsLastMonth int `json:"totalDeploymentsLastMonth"`
	TotalLogsLastMonth        int `json:"totalLogsLastMonth"`
	TotalRequestsLastMonth    int `json:"totalRequestsLastMonth"`
}

type GraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type GraphQLResponse struct {
	Data   map[string]interface{}   `json:"data"`
	Errors []map[string]interface{} `json:"errors,omitempty"`
}

func GetPublicStats() (*PublicStats, error) {
	query := `
	query publicStats {
		publicStats {
			...PublicStatsFields
		}
	}
	fragment PublicStatsFields on PublicStats {
		totalUsers
		totalProjects
		totalServices
		totalDeploymentsLastMonth
		totalLogsLastMonth
		totalRequestsLastMonth
	}
	`

	requestBody := GraphQLRequest{
		Query: query,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		log.Fatalf("Error marshaling request: %v", err)
	}

	req, err := http.NewRequest("POST", RAILWAY_API_URL, bytes.NewBuffer(jsonData))
	if err != nil {
		log.Fatalf("Error creating request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("Error making request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Unexpected status code: %d, body: %s", resp.StatusCode, body)
	}

	var graphQLResponse GraphQLResponse
	if err := json.Unmarshal(body, &graphQLResponse); err != nil {
		log.Fatalf("Error unmarshaling response: %v", err)
	}

	if len(graphQLResponse.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL errors: %v", graphQLResponse.Errors)
	}

	publicStatsData := graphQLResponse.Data["publicStats"]
	publicStatsJSON, err := json.Marshal(publicStatsData)
	if err != nil {
		log.Fatalf("Error re-marshaling public stats: %v", err)
	}

	var publicStats PublicStats
	if err := json.Unmarshal(publicStatsJSON, &publicStats); err != nil {
		log.Fatalf("Error unmarshaling public stats: %v", err)
	}

	return &publicStats, nil
}
