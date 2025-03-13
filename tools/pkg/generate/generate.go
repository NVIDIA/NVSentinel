// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/go-logr/logr"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func getClient(log logr.Logger, config *oauth2.Config) *http.Client {
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		tok = getTokenFromWeb(log, config)
		saveToken(log, tokenFile, tok)
	}
	return config.Client(context.Background(), tok)
}

func getTokenFromWeb(log logr.Logger, config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	log.Info("URL: " + authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Error(err, "Unable to read authorization code")
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Error(err, "Unable to retrieve token from web")
	}
	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(log logr.Logger, path string, token *oauth2.Token) {
	log.Info("Saving credential file to: " + path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Error(err, "Unable to cache oauth token")
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func Generate(log logr.Logger, credentialsPath, gpuHealthMonitorXIDPath, nvSwitchHealthMonitorSXIDPath string) error {
	ctx := context.Background()
	b, err := os.ReadFile(credentialsPath)
	if err != nil {
		log.Error(err, "Unable to read client secret file")
		return fmt.Errorf("unable to read client secret file: %w", err)
	}

	config, err := google.ConfigFromJSON(b, spreadsheetScope)
	if err != nil {
		log.Error(err, "Unable to parse client secret file to config")
		return fmt.Errorf("unable to parse client secret file to config: %w", err)
	}

	client := getClient(log, config)

	srv, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Error(err, "Unable to retrieve Sheets client")
		return fmt.Errorf("unable to retrieve Sheets client: %w", err)
	}

	spreadsheet, err := srv.Spreadsheets.Get(spreadsheetId).Do()
	if err != nil {
		log.Error(err, "Unable to retrieve data from sheet")
		return fmt.Errorf("unable to retrieve data from sheet: %w", err)
	}

	var sheetName string
	for _, sheet := range spreadsheet.Sheets {
		if sheet.Properties.SheetId == targetGID {
			sheetName = sheet.Properties.Title
			break
		}
	}

	if sheetName == "" {
		log.Error(fmt.Errorf("sheet with given GID not found"), "Sheet with given GID not found")
		return fmt.Errorf("sheet with given GID not found")
	}

	readRange := sheetName
	resp, err := srv.Spreadsheets.Values.Get(spreadsheetId, readRange).Do()
	if err != nil {
		log.Error(err, "Unable to retrieve data from sheet")
		return fmt.Errorf("unable to retrieve data from sheet: %w", err)
	}

	var sxidConfigMapData, xidConfigMapData string

	for _, row := range resp.Values {
		recommendedActions := row[13].(string)
		fatality := categorizeXID(recommendedActions)

		if recommendedActions == "" {
			row[13] = "REPORT_ISSUE"
			fatality = categorizeXID("REPORT_ISSUE")
		}

		log.Info(fmt.Sprintf("%s: %s, %s, %s, %s", row[0], row[1], row[2], row[13], fatality))
		if row[0] == "XID" {
			xidConfigMapData = fmt.Sprintf("%s%s,%s,%s,%s\n", xidConfigMapData, row[1], row[2], row[13], fatality)
		} else if row[0] == "SXID" {
			sxidConfigMapData = fmt.Sprintf("%s%s,%s,%s,%s\n", sxidConfigMapData, row[1], row[2], row[13], fatality)
		} else {
			log.Error(fmt.Errorf("invalid row"), "Invalid row")
			return fmt.Errorf("invalid row")
		}
	}

	err = os.WriteFile(gpuHealthMonitorXIDPath, []byte(xidConfigMapData), 0644)
	if err != nil {
		log.Error(err, "Unable to write gpu health monitor XID values")
		return fmt.Errorf("unable to write gpu health monitor XID values: %w", err)
	}

	err = os.WriteFile(nvSwitchHealthMonitorSXIDPath, []byte(sxidConfigMapData), 0644)
	if err != nil {
		log.Error(err, "Unable to write nv switch health monitor SXID values")
		return fmt.Errorf("unable to write nv switch health monitor SXID values: %w", err)
	}

	return nil
}

func categorizeXID(recommendedActions string) Fatality {
	isFatal := map[string]bool{
		"UNEXPECTED_ERR_REPORT_ISSUE":          true,
		"WORKFLOW_NVLINK_POTENTIALY_FATAL_ERR": true,
		"RESET_FABRIC":                         true,
		"RESET_GPU":                            true,
		"RUN_FIELDDIAG":                        true,
		"CHECK_LINK_MECHANICAL_CONNECTIONS":    true,
		"INVESTIGATE_LINK_SI":                  true,
		"CHECK_FM_CONFIG":                      true,
		"CHECK_THERMALS":                       true,
		"REPORT_ISSUE":                         true,
		"CHECK_MECHANICALS":                    true,
		"RESTART_BM":                           true,
		"RESTART_VM":                           true,
		"UPDATE_SWFW":                          true,
		"WORKFLOW_NVLINK5_ERR":                 true,
		"WORKFLOW_NVLINK_ERR":                  true,
		"WORKFLOW_XID_48":                      true,
	}

	if isFatal[recommendedActions] {
		return fatal
	}
	return nonFatal
}
