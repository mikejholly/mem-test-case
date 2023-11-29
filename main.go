package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/system"
	"golang.org/x/sync/errgroup"
)

func buildkitAddr() string {
	buildkitAddr := os.Getenv("BUILDKIT_ADDR")
	if buildkitAddr == "" {
		buildkitAddr = "tcp://127.0.0.1:8372"
	}
	return buildkitAddr
}

func main() {
	bkClient, err := client.New(context.TODO(), buildkitAddr())
	if err != nil {
		fmt.Printf("failed to connect to buildkit: %v\n", err)
		os.Exit(1)
	}
	defer bkClient.Close()

	ch := make(chan *client.SolveStatus)
	go logStatus(ch)

	buildFunc := func(ctx context.Context, gwClient gwclient.Client) (*gwclient.Result, error) {

		baseStage := llb.Image("docker.io/library/golang:1.17-alpine").
			AddEnv("PATH", "/usr/local/go/bin:"+system.DefaultPathEnvUnix).
			File(llb.Mkdir("/opt", os.ModeDir)).
			Dir("/opt").
			File(llb.Copy(llb.Local("src"), "hello-world/main.go", ".")).
			File(llb.Copy(llb.Local("src"), "hello-world/go.mod", ".")).
			Run(llb.Shlex("go build -o hello-world main.go")).Root()

		testStage := baseStage.
			Run(llb.Shlex("go test ./...")).Root()

		result := gwclient.NewResult()

		def, err := testStage.Marshal(ctx)
		if err != nil {
			return nil, err
		}

		r, err := gwClient.Solve(ctx, gwclient.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}

		ref, err := r.SingleRef()
		if err != nil {
			return nil, err
		}

		eg, solveCtx := errgroup.WithContext(ctx)

		eg.SetLimit(5)

		idx := atomic.Int32{}

		for i := 0; i < 100; i++ {
			eg.Go(func() error {
				ctx := solveCtx

				start, err := ref.ToState()
				if err != nil {
					return err
				}

				buildStage := start.
					Run(llb.Shlex("go build -o hello-world main.go")).Root()

				appStage := llb.Scratch().
					AddEnv("PATH", "/bin").
					File(llb.Mkdir("/bin", os.ModeDir)).
					File(llb.Copy(buildStage, "/opt/hello-world", "/bin/"))

				def, err := appStage.Marshal(ctx)
				if err != nil {
					return err
				}

				r, err := gwClient.Solve(ctx, gwclient.SolveRequest{
					Definition: def.ToPB(),
				})
				if err != nil {
					return err
				}

				ref, err := r.SingleRef()
				if err != nil {
					return err
				}

				data, err := ref.ReadFile(ctx, gwclient.ReadRequest{
					Filename: "/opt/hello-world",
				})
				if err != nil {
					return fmt.Errorf("error reading file from reference: %w", err)
				}

				fmt.Printf("File length: %q\n", len(data))

				idx.Add(1)
				result.AddRef(fmt.Sprintf("ref_%d", idx.Load()), ref)

				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			return nil, err
		}

		return result, nil
	}

	solveOpt := client.SolveOpt{}

	resp, err := bkClient.Build(context.TODO(), solveOpt, "", buildFunc, ch)
	if err != nil {
		fmt.Printf("failed to build llb: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Solve Response: %v\n", resp)
}

func logStatus(ch chan *client.SolveStatus) {
	for {
		status := <-ch
		if status == nil {
			break
		}
		fmt.Printf("status is %v\n", status)
		for _, v := range status.Vertexes {
			fmt.Printf("====vertex: %+v\n", v)
		}
		for _, s := range status.Statuses {
			fmt.Printf("====status: %+v\n", s)
		}
		for _, l := range status.Logs {
			fmt.Printf("====log: %s\n", string(l.Data))
		}
	}
}
