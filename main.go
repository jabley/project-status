package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"

	"net/http"
	_ "net/http/pprof"
)

const usage = `
GH_ORG=MyOrgOrUser GH_OAUTH_TOKEN=MyToken project-status [-help] [-empty] [-debug]

By default, project-status will check that all repositories have a README.

You can choose other behaviour:
`

// RepositorySummary is the model of our evaluation of a repository health.
type RepositorySummary struct {
	Repository github.Repository
	HasReadme  bool
}

func (rs RepositorySummary) String() string {
	return fmt.Sprintf("[name=%s, HasReadme=%v]", *rs.Repository.Name, rs.HasReadme)
}

// EmptyRepository defines a Stringer that indicates whether a repository is empty or not.
// This is useful for finding repositories that have been created, but not actually committed to.
// Yes, really.
type EmptyRepository struct {
	Repository github.Repository
}

func (er EmptyRepository) String() string {
	return fmt.Sprintf("[name=%s, PushedAt=%v, CreatedAt:=%v, Size=%v]",
		*er.Repository.Name,
		er.Repository.PushedAt,
		er.Repository.CreatedAt,
		*er.Repository.Size,
	)
}

// workerFn is a function that will typically return a closure that can be used as a Callable by a Job.
// See readme for an example of such a function.
// It is responsible for sending a struct on the provided done channel to notify the client that it's finished.
// I tried to do this passing the sync.WaitGroup, but that just hung. Some subtlety of the golang memory model
// that I've not grokked yet.
type workerFn func(done chan struct{}, client *github.Client, repo github.Repository, out chan fmt.Stringer) func()

// Job represents the job to be run. This is an abstraction to control how many
type Job struct {
	// Callable is the unit of work that should be wrapped.
	Callable func()
}

// Worker represents the worker that executes the job
type Worker struct {
	WorkerPool chan chan Job
	JobChannel chan Job
	quit       chan struct{}
}

// NewWorker creates a new Worker which wraps the provided workerPool.
func NewWorker(workerPool chan chan Job) Worker {
	return Worker{
		WorkerPool: workerPool,
		JobChannel: make(chan Job),
		quit:       make(chan struct{})}
}

// Start method starts the run loop for the worker, listening for a quit channel in
// case we need to stop it
func (w Worker) Start() {
	go func() {
		for {
			// register the current worker into the worker queue.
			w.WorkerPool <- w.JobChannel

			select {
			case job := <-w.JobChannel:
				// we have received a work request.
				// Do the work
				job.Callable()

			case <-w.quit:
				// we have received a signal to stop
				return
			}
		}
	}()
}

// Stop signals the worker to stop listening for work requests.
func (w Worker) Stop() {
	go func() {
		w.quit <- struct{}{}
	}()
}

// Dispatcher is a throttling mechanism so that we don't make too many requests to
// HTTP servers (and thus run out of file handles, slow down too much).
type Dispatcher struct {
	// A pool of workers channels that are registered with the dispatcher
	WorkerPool chan chan Job
	maxWorkers int
	jobQueue   chan Job
}

// NewDispatcher creates a Dispatcher with the specified number of workers.
func NewDispatcher(maxWorkers int, jobQueue chan Job) *Dispatcher {
	pool := make(chan chan Job, maxWorkers)
	return &Dispatcher{WorkerPool: pool,
		maxWorkers: maxWorkers,
		jobQueue:   jobQueue,
	}
}

// Run starts all the workers and runs the select loop to do work.
func (d *Dispatcher) Run() {
	// starting n number of workers
	for i := 0; i < d.maxWorkers; i++ {
		worker := NewWorker(d.WorkerPool)
		worker.Start()
	}

	go d.dispatch()
}

// signal sends an empty struct to the specified done channel. This is provided
// since `defer` operates on functions rather than expressions.
func signal(done chan struct{}) {
	done <- struct{}{}
}

func (d *Dispatcher) dispatch() {
	for {
		select {
		case job := <-d.jobQueue:
			// a job request has been received
			// fmt.Printf("Received a job request\n")

			go func(job Job) {
				// try to obtain a worker job channel that is available.
				// this will block until a worker is idle
				// fmt.Printf("Retrieving a worker\n")
				jobChannel := <-d.WorkerPool

				// dispatch the job to the worker job channel
				// fmt.Printf("Dispatching the job to the worker\n")
				jobChannel <- job
			}(job)
		}
	}
}

// readme returns a workerFn that checks a repository has a README
func readme(org string) workerFn {
	return func(done chan struct{}, client *github.Client, repo github.Repository, out chan fmt.Stringer) func() {
		return func() {
			defer signal(done)
			readme, resp, err := client.Repositories.GetReadme(org, *repo.Name, nil)
			out <- RepositorySummary{Repository: repo,
				HasReadme: err == nil && readme != nil,
			}
			httpCleanup(resp)
		}
	}
}

// emptyRepos returns a workerFn that checks whether a repository has had any commits.
func emptyRepos(org string) workerFn {
	return func(done chan struct{}, client *github.Client, repo github.Repository, out chan fmt.Stringer) func() {
		return func() {
			defer signal(done)
			if *repo.PushedAt == *repo.CreatedAt {
				out <- EmptyRepository{Repository: repo}
			}
		}
	}
}

const (
	debugUsage   = "debug this process at http://localhost:6060/debug/pprof/"
	debugDefault = false

	emptyUsage   = "check for empty repositories"
	emptyDefault = false

	helpUsage   = "display the usage message and exit"
	helpDefault = false
)

func main() {
	var (
		debug, empty, help bool
	)

	flag.BoolVar(&debug, "debug", debugDefault, debugUsage)
	flag.BoolVar(&debug, "d", debugDefault, debugUsage)
	flag.BoolVar(&empty, "empty", emptyDefault, emptyUsage)
	flag.BoolVar(&empty, "e", emptyDefault, emptyUsage)
	flag.BoolVar(&help, "help", helpDefault, helpUsage)
	flag.BoolVar(&help, "h", helpDefault, helpUsage)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage)
		flag.PrintDefaults()
	}

	oauthToken := os.Getenv("GH_OAUTH_TOKEN")

	if oauthToken == "" {
		showUsage()
		os.Exit(1)
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: oauthToken},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)

	client := github.NewClient(tc)

	org := os.Getenv("GH_ORG")

	if org == "" {
		showUsage()
		os.Exit(1)
	}

	flag.Parse()

	if help {
		showUsage()
		os.Exit(2)
	}

	if debug {
		go func() {
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()
	}

	jobQueue := make(chan Job)
	dispatcher := NewDispatcher(10, jobQueue)
	dispatcher.Run()

	allRepos, err := getAllRepos(client, org)

	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(3)
	}

	fmt.Printf("Got %v repositories\n", len(allRepos))

	// default aggregator function
	aggregatorFn := readme(org)

	if empty {
		aggregatorFn = emptyRepos(org)
	}

	// run our aggregating report function on all repositories and print the result
	for summary := range merge(jobQueue, client, allRepos, aggregatorFn) {
		fmt.Printf("%v\n", summary)
	}
}

func showUsage() {
	flag.Usage()
}

func merge(jobQueue chan Job, client *github.Client, repos []github.Repository, fn workerFn) <-chan fmt.Stringer {
	var wg sync.WaitGroup
	out := make(chan fmt.Stringer)

	for _, repo := range repos {
		wg.Add(1)
		// fmt.Printf("Creating job for %v\n", *repo.Name)
		done := make(chan struct{})
		work := Job{Callable: fn(done, client, repo, out)}
		// fmt.Printf("Submitting job for %v\n", *repo.Name)
		jobQueue <- work

		// FIXME(jabley): this feels a bit ugly. Is there a simpler way?
		go func(done chan struct{}) {
			select {
			case <-done:
				wg.Done()
			}
		}(done)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func getAllRepos(client *github.Client, org string) ([]github.Repository, error) {
	opt := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 40},
	}

	// get all pages of results
	var allRepos []github.Repository
	for {
		repos, resp, err := client.Repositories.ListByOrg(org, opt)
		if err != nil {
			return nil, err
		}
		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
		httpCleanup(resp)
	}

	return allRepos, nil
}

func httpCleanup(resp *github.Response) {
	if resp == nil {
		return
	}

	if resp.Body == nil {
		return
	}

	resp.Body.Close()
}
