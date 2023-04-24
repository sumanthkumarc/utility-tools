package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/hashicorp/vault/api"
)

// @todo add as parameter
const (
	VAULT_URL   = "<VAULT_URL>"
	VAULT_TOKEN = "<VAULT_TOKEN>"
)

// Create a new AWS session
var sess client.ConfigProvider = session.Must(session.NewSession(
	&aws.Config{
		// @todo add as parameter or automatic discovery
		Region:      aws.String("<AWS_REGION>"),
		Credentials: credentials.NewSharedCredentials("", "<AWS_PROFILE>"),
	},
))

var svc *ssm.SSM = ssm.New(sess)

func main() {
	SSMData := make(map[string]string)

	// Create a new Vault API client
	config := api.DefaultConfig()
	config.Address = VAULT_URL

	client, err := api.NewClient(config)
	if err != nil {
		panic(err)
	}

	client.SetToken(VAULT_TOKEN)

	// Retrieve a list of all mount points
	mounts := GetMounts(client)
	if err != nil {
		panic(err)
	}

	// @todo use flag to determine custom paths ot automatic path discovery and retrieval
	// mounts := map[string]string{
	// 	"my-path": "kv1",
	// }

	// Loop through all mount points and list all paths and their values under each mount point
	// @todo one go routine per mount to make things faster. Also add a channel to deliver the
	// mount outputs once it is done and SSM create script can read it as and when it arrives
	for path, backendType := range mounts {
		MountData := make(map[string]string)
		mountPath := path + "/"

		if err := listRecursive(client, mountPath, backendType, MountData); err != nil {
			panic(err)
		}

		mergeMap(MountData, SSMData)
		// fmt.Println(SSMData)
	}

	writeToParamStore(SSMData)
}

func listRecursive(client *api.Client, path string, backendType string, KVData map[string]string) error {

	// Create a new request to list all paths under the given path
	listRequest := client.NewRequest("LIST", "")

	if backendType == "kv1" {
		// For KV version 1 backends
		listRequest.URL.Path = fmt.Sprintf("/v1/%s", path)
	} else if backendType == "kv2" {
		// For KV version 2 backends, the secrets are listed using the "data" endpoint
		listRequest.URL.Path = fmt.Sprintf("/v1/%s/data?list=true", path)
	} else {
		return fmt.Errorf("unknown backend type %s for path %s", backendType, path)
	}

	// Send the request to Vault
	response, err := client.RawRequest(listRequest)
	if err != nil {
		return err
	}

	// Parse the response body as a JSON object
	var result struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := response.DecodeJSON(&result); err != nil {
		return err
	}

	// fmt.Println(result.Data.Keys)

	// Recursively list all paths and their values under each sub-path
	for _, key := range result.Data.Keys {
		subPath := path + key
		if key[len(key)-1] == '/' {
			// If the key is a folder, recursively list its contents
			if err := listRecursive(client, subPath, backendType, KVData); err != nil {
				return err
			}
		} else {
			// If the key is a file, read its value and print the path and value
			secret, err := readSecret(client, subPath, backendType)
			if err != nil {
				return err
			}
			if secret != nil && secret.Data != nil {

				// handle simple key value pairs and json blobs
				if secret.Data["value"] != nil {
					value, ok := secret.Data["value"].(string)
					if ok {
						// fmt.Printf("%s %s\n", subPath, value)
						KVData[subPath] = value
					}
				} else {
					// @todo think if we wanna convert json blobs to simple kv's
					value, err := json.Marshal(secret.Data)
					if err != nil {
						return err
					} else {
						// fmt.Printf("%s %s\n", subPath, value)
						KVData[subPath] = string(value[:])
					}
				}
			}
		}
	}

	return nil
}

func GetMounts(client *api.Client) map[string]string {

	secret, err := client.Sys().ListMounts()

	if err != nil {
		log.Fatalf("%v", err)
	}

	mounts := make(map[string]string)
	var EngineType string
	for mount, metadata := range secret {
		if metadata.Type != "generic" && metadata.Type != "kv" {
			continue
		}

		// Generic -> kv1 or if kv depending on version
		if metadata.Type == "generic" {
			EngineType = "kv1"
		} else {
			if metadata.Options["version"] == "1" {
				EngineType = "kv1"
			} else {
				EngineType = "kv2"
			}
		}

		mounts[mount] = EngineType
	}

	if mounts == nil {
		log.Fatalf("No mounts found or your token has no access.")
	}

	return mounts
}

func readSecret(client *api.Client, path string, backendType string) (*api.KVSecret, error) {

	if backendType == "kv1" {
		paths := strings.SplitN(path, "/", 2)
		secret, err := client.KVv1(paths[0]).Get(context.TODO(), paths[1])
		if err != nil {
			return nil, err
		}
		return secret, nil
	} else if backendType == "kv2" {
		mountPath := strings.Split(path, "/")[0]
		secret, err := client.KVv2(mountPath).Get(context.TODO(), path)
		if err != nil {
			return nil, err
		}
		return secret, nil
	} else {
		return nil, fmt.Errorf("unknown backend type %s for path %s", backendType, path)
	}
}

func mergeMap(source map[string]string, destination map[string]string) {
	for k, v := range source {
		destination[k] = v
	}
}

func writeToParamStore(data map[string]string) {

	for key, value := range data {
		input := &ssm.PutParameterInput{
			// the parameter store keys should always start with /
			Name:  aws.String(fmt.Sprintf("/%s", key)),
			Value: aws.String(value),
			Type:  aws.String("String"),
		}

		_, err := svc.PutParameter(input)
		if err != nil {
			fmt.Println(err.Error())
		}

		fmt.Printf("Parameter '%s' created successfully.\n", key)
	}
}
