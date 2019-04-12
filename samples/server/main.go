package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"go.opencensus.io/trace"
	"gocloud.dev/gcp"
	"gocloud.dev/server"
	"gocloud.dev/server/sdserver"
)

type GlobalMonitoredResource struct {
	projectId string
}

func (g GlobalMonitoredResource) MonitoredResource() (string, map[string]string) {
	return "global", map[string]string{"project_id": g.projectId}
}

func helloHandler(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "Hello\n")
}

func mainHandler(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "Welcome to the home page!\n")
}

func main() {
	addr := flag.String("listen", ":8080", "HTTP port to listen on")
	doTrace := flag.Bool("trace", true, "Export traces to StackDriver")

	ctx := context.Background()
	credentials, err := gcp.DefaultCredentials(ctx)

	if err != nil {
		log.Fatal(err)
	}
	tokenSource := gcp.CredentialsTokenSource(credentials)
	projectId, err := gcp.DefaultProjectID(credentials)
	if err != nil {
		log.Fatal(err)
	}

	var exporter *stackdriver.Exporter = nil
	if *doTrace {
		fmt.Println("Exporting traces to StackDriver")
		mr := GlobalMonitoredResource{projectId: string(projectId)}
		exporter, _, err = sdserver.NewExporter(projectId, tokenSource, mr)
		if err != nil {
			log.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", helloHandler)
	mux.HandleFunc("/", mainHandler)

	options := &server.Options{
		RequestLogger:         sdserver.NewRequestLogger(),
		HealthChecks:          nil,
		TraceExporter:         exporter,
		DefaultSamplingPolicy: trace.AlwaysSample(),
		Driver:                &server.DefaultDriver{},
	}

	s := server.New(mux, options)
	fmt.Printf("Listening on %s\n", *addr)
	s.ListenAndServe(*addr)
}
