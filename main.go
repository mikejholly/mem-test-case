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

func buildLLB(ctx context.Context, gwClient gwclient.Client, st *llb.State) (*llb.State, error) {
	def, err := st.Marshal(ctx)
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

	_, err = ref.ReadDir(ctx, gwclient.ReadDirRequest{Path: "/"})
	if err != nil {
		return nil, err
	}

	return st, nil
}

func baseState(ctx context.Context, gwClient gwclient.Client) (*llb.State, error) {
	a := llb.Image("docker.io/library/golang:1.17-alpine").
		AddEnv("PATH", "/usr/local/go/bin:"+system.DefaultPathEnvUnix).
		File(llb.Mkdir("/opt", os.ModeDir)).
		Dir("/opt").
		File(llb.Copy(llb.Local("src"), "hello-world/main.go", ".")).
		File(llb.Copy(llb.Local("src"), "hello-world/go.mod", ".")).
		Run(llb.Shlex("go build -o hello-world main.go")).
		Root()

	_, err := buildLLB(ctx, gwClient, &a)
	if err != nil {
		return nil, err
	}

	last := a
	for i := 0; i < 200; i++ {
		cmd := fmt.Sprintf("echo hello-world-%d", i)
		last = last.Run(llb.Shlex(cmd)).Root()
	}

	return &last, err
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

		result := gwclient.NewResult()

		idx := atomic.Int32{}

		eg, ctx := errgroup.WithContext(ctx)

		eg.SetLimit(10)

		for i := 0; i < 1000; i++ {
			eg.Go(func() error {
				st, err := baseState(ctx, gwClient)
				if err != nil {
					return err
				}

				file := fmt.Sprintf("/tmp/%d.txt", idx.Load())

				appStage := llb.Image("docker.io/library/golang:1.17-alpine").
					File(llb.Mkdir(file, os.ModeDir)).
					Run(llb.Shlex("echo hi > " + file)).
					Run(llb.Shlex("ls -l " + file)).
					File(llb.Copy(*st, "/opt/hello-world", file))

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

				_, err = ref.ReadDir(ctx, gwclient.ReadDirRequest{Path: "/"})
				if err != nil {
					return err
				}

				idx.Add(1)
				result.AddRef(fmt.Sprintf("ref_%d", idx.Load()), ref)

				return nil
			})
		}

		err := eg.Wait()
		if err != nil {
			return nil, err
		}

		return result, nil
	}

	localPath, err := os.Getwd()
	if err != nil {
		fmt.Printf("failed to current path: %v\n", err)
		os.Exit(1)
	}

	solveOpt := client.SolveOpt{
		LocalDirs: map[string]string{
			"src": localPath,
		},
	}

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
		for _, v := range status.Vertexes {
			if v.Cached {
				fmt.Printf("CACHED ")
			}
			fmt.Printf("%s\n", v.Name)
		}
	}
}
