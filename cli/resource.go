package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/go-docopt"
)

func init() {
	register("resource", runResource, `
usage: flynn resource
       flynn resource add <provider>
       flynn resource remove <provider> <resource>
       flynn resource tunables <provider> <resource>
       flynn resource tunables get <provider> <resource> <var>
       flynn resource tunables set <provider> <resource> <var>=<val>...
       flynn resource tunables unset <provider> <resource> <var>...

Manage resources for the app.

Commands:
       With no arguments, shows a list of resources.

       add     provisions a new resource for the app using <provider>.
       remove  removes the existing <resource> provided by <provider>.
`)
}

func runResource(args *docopt.Args, client controller.Client) error {
	if args.Bool["add"] {
		return runResourceAdd(args, client)
	}
	if args.Bool["remove"] {
		return runResourceRemove(args, client)
	}
	if args.Bool["tunables"] {
		return runResourceTunables(args, client)
	}

	resources, err := client.AppResourceList(mustApp())
	if err != nil {
		return err
	}

	w := tabWriter()
	defer w.Flush()

	var provider *ct.Provider

	listRec(w, "ID", "Provider ID", "Provider Name")
	for _, j := range resources {
		provider, err = client.GetProvider(j.ProviderID)
		if err != nil {
			return err
		}
		listRec(w, j.ID, j.ProviderID, provider.Name)
	}

	return err
}

func runResourceAdd(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]

	res, err := client.ProvisionResource(&ct.ResourceReq{ProviderID: provider, Apps: []string{mustApp()}})
	if err != nil {
		return err
	}

	env := make(map[string]*string)
	for k, v := range res.Env {
		s := v
		env[k] = &s
	}

	releaseID, err := setEnv(client, "", env)
	if err != nil {
		return err
	}

	log.Printf("Created resource %s and release %s.", res.ID, releaseID)

	return nil
}

// TODO(jpg): Automatic resource selection given provider name.
// We should lookup the provider to get it's ID and then if this identifies a resource
// without ambiguity we should use that instead of prompting user for resource ID
func runResourceRemove(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]
	resource := args.String["<resource>"]

	res, err := client.DeleteResource(provider, resource)
	if err != nil {
		return err
	}

	release, err := client.GetAppRelease(mustApp())
	if err != nil {
		return err
	}

	// Unset all the keys associated with this resource
	env := make(map[string]*string)
	for k := range res.Env {
		// Only unset the key if it hasn't been modified
		if release.Env[k] == res.Env[k] {
			env[k] = nil
		}
	}

	releaseID, err := setEnv(client, "", env)
	if err != nil {
		return err
	}

	log.Printf("Deleted resource %s, created release %s.", res.ID, releaseID)

	return nil
}

func runResourceTunables(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]
	resource := args.String["<resource>"]

	if args.Bool["set"] {
		return runResourceTunablesSet(args, client)
	} else if args.Bool["get"] {
		return runResourceTunablesGet(args, client)
	} else if args.Bool["unset"] {
		return runResourceTunablesUnset(args, client)
	}

	tunables, err := client.GetResourceTunables(provider, resource)
	if err != nil {
		return err
	}

	// TODO(jpg) revisit display
	vars := make([]string, 0, len(tunables.Data))
	for k, v := range tunables.Data {
		vars = append(vars, k+"="+v)
	}
	sort.Strings(vars)

	for _, v := range vars {
		fmt.Println(v)
	}

	return nil
}

func runResourceTunablesGet(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]
	resource := args.String["<resource>"]
	arg := args.All["<var>"].([]string)[0]

	tunables, err := client.GetResourceTunables(provider, resource)
	if err != nil {
		return err
	}

	if v, ok := tunables.Data[arg]; ok {
		fmt.Println(v)
		return nil
	}

	return fmt.Errorf("tunable %s not set", arg)
}
func runResourceTunablesSet(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]
	resource := args.String["<resource>"]

	tunables, err := client.GetResourceTunables(provider, resource)
	if err != nil {
		return err
	}

	pairs := args.All["<var>=<val>"].([]string)
	newTunables := make(map[string]*string, len(pairs))
	for _, s := range pairs {
		v := strings.SplitN(s, "=", 2)
		if len(v) != 2 {
			return fmt.Errorf("invalid tunable format: %q", s)
		}
		newTunables[v[0]] = &v[1]
	}

	for k, v := range newTunables {
		if v == nil {
			delete(tunables.Data, k)
		} else {
			tunables.Data[k] = *v
		}
	}

	// Bump version
	tunables.Version++

	err = client.UpdateResourceTunables(provider, resource, tunables)
	if err != nil {
		return err
	}
	log.Printf("Updated resource tunables")
	return nil
}
func runResourceTunablesUnset(args *docopt.Args, client controller.Client) error {
	provider := args.String["<provider>"]
	resource := args.String["<resource>"]

	tunables, err := client.GetResourceTunables(provider, resource)
	if err != nil {
		return err
	}

	vars := args.All["<var>"].([]string)
	for _, s := range vars {
		delete(tunables.Data, s)
	}

	// Bump version
	tunables.Version++

	err = client.UpdateResourceTunables(provider, resource, tunables)
	if err != nil {
		return err
	}
	log.Printf("Updated resource tunables")
	return nil
}
