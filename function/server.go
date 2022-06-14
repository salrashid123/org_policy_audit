package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sync"

	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"golang.org/x/time/rate"

	"golang.org/x/net/context"
	"golang.org/x/net/http2"
	orgpb "google.golang.org/genproto/googleapis/cloud/orgpolicy/v2"
	// "google.golang.org/protobuf/encoding/protojson"
	// "github.com/golang/protobuf/jsonpb"
)

type bqRequest struct {
	RequestId          string            `json:"requestId"`
	Caller             string            `json:"caller"`
	SessionUser        string            `json:"sessionUser"`
	UserDefinedContext map[string]string `json:"userDefinedContext"`
	Calls              [][]interface{}   `json:"calls"`
}

type bqResponse struct {
	Replies      []string `json:"replies,omitempty"`
	ErrorMessage string   `json:"errorMessage,omitempty"`
}

const (

	// https://cloud.google.com/asset-inventory/docs/supported-asset-types#analyzable_asset_types
	assetTypePolicy    = "orgpolicy.googleapis.com/Policy"
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

	maxRequestsPerSecond float64 = 50 // "golang.org/x/time/rate" limiter to throttle operations
	burst                int     = 4
)

var (
	limiter *rate.Limiter
)

func init() {

	limiter = rate.NewLimiter(rate.Limit(maxRequestsPerSecond), burst)

}

func GET_EFFECTIVE_POLICY(w http.ResponseWriter, r *http.Request) {

	bqReq := &bqRequest{}
	bqResp := &bqResponse{}

	if err := json.NewDecoder(r.Body).Decode(&bqReq); err != nil {
		bqResp.ErrorMessage = fmt.Sprintf("External Function error: can't read POST body %v", err)
	} else {

		fmt.Printf("caller %s\n", bqReq.Caller)
		fmt.Printf("sessionUser %s\n", bqReq.SessionUser)
		fmt.Printf("userDefinedContext %v\n", bqReq.UserDefinedContext)

		wait := new(sync.WaitGroup)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		objs := make([]string, len(bqReq.Calls))

		orgClient, err := orgpolicy.NewClient(ctx)
		if err != nil {
			fmt.Printf("Error creating new org client%v\n", err)
			return
		}
		defer orgClient.Close()

		for i, r := range bqReq.Calls {
			if len(r) != 1 {
				bqResp.ErrorMessage = fmt.Sprintf("Invalid number of input fields provided.  expected 1, got  %d", len(r))
			}
			name, ok := r[0].(string)
			if !ok {
				bqResp.ErrorMessage = "Invalid resource type. expected string"
			}
			if bqResp.ErrorMessage != "" {
				bqResp.Replies = nil
				break
			}

			//  use goroutines heres but keep the order
			wait.Add(1)
			go func(j int) {
				defer wait.Done()
				for {
					select {
					case <-ctx.Done():
						return
					default:

						var err error
						if err := limiter.Wait(ctx); err != nil {
							fmt.Printf("Error in rate limiter %v\n", err)
							bqResp.ErrorMessage = fmt.Sprintf("Error in rate limiter for row %d, [%v]", j, err)
							bqResp.Replies = nil
							cancel()
							return
						}
						if ctx.Err() != nil {
							fmt.Printf("Error in rate limiter %v\n", err)
							bqResp.ErrorMessage = fmt.Sprintf("Error in rate limiter for row %d, [%v]", j, err)
							bqResp.Replies = nil
							cancel()
							return
						}
						resp, err := orgClient.GetEffectivePolicy(ctx, &orgpb.GetEffectivePolicyRequest{
							Name: name,
						})
						if err != nil {
							bqResp.ErrorMessage = fmt.Sprintf("Error getting effective policy for row %d, [%v]", j, err)
							bqResp.Replies = nil
							cancel()
							return
						}
						fmt.Printf("            effective policy for [%s] is %v\n", name, resp.Spec.Rules)

						var buffer bytes.Buffer
						// https://pkg.go.dev/google.golang.org/genproto/googleapis/cloud/orgpolicy/v2#PolicySpec
						err = json.NewEncoder(&buffer).Encode(resp.Spec)
						if err != nil {
							bqResp.ErrorMessage = fmt.Sprintf("Error encoding  PolicySpec_PolicyRule to json for row %d, [%v]", j, err)
							bqResp.Replies = nil
							cancel()
							return
						}
						objs[j] = strings.TrimSuffix(fmt.Sprintf("%v", buffer.String()), "\n")
						return
					}
				}
			}(i)
		}
		wait.Wait()
		if bqResp.ErrorMessage != "" {
			bqResp.Replies = nil
		} else {
			bqResp.Replies = objs
		}
	}

	b, err := json.Marshal(bqResp)
	if err != nil {
		http.Error(w, fmt.Sprintf("can't convert response to JSON %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func main() {

	http.HandleFunc("/", GET_EFFECTIVE_POLICY)

	server := &http.Server{
		Addr: ":8080",
	}
	http2.ConfigureServer(server, &http2.Server{})
	fmt.Println("Starting Server..")
	err := server.ListenAndServe()
	panic(fmt.Errorf("unable to start Server %v", err))
}
