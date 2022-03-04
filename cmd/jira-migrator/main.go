package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	jira "github.com/andygrunwald/go-jira"
	cli "github.com/urfave/cli/v2"
	yaml "gopkg.in/yaml.v2"
)

var (
	DefaultSearchFields = []string{
		"project",
		"sprint", // Doesn't seem to do anything
		"summary",
		"status",
		"description",
		"created",
		"creator",
		"updated",
		"resolutiondate",
		"issuelinks",
		"issuetype",
		"labels",
		"assignee",
		"reporter",
		"comment",
		"attachment",
		"priority",
		"parent",
		"subtasks",
		"customfield_10620", // epic key
		"customfield_10621", // epic name
		"customfield_10222", // sprint (only for Jira Server)
		/*
				customfield_10222": [
			      "com.atlassian.greenhopper.service.sprint.Sprint@1da092b0[id=9090,rapidViewId=1151,state=CLOSED,name=Apple Platz,startDate=2021-11-11T20:11:47.619Z,endDate=2021-11-25T23:11:00.000Z,completeDate=2021-12-02T19:41:10.134Z,sequence=9090,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@2076d393[id=9192,rapidViewId=1151,state=CLOSED,name=Black Forest,startDate=2021-12-02T20:13:17.127Z,endDate=2021-12-16T23:13:00.000Z,completeDate=2021-12-16T19:51:01.534Z,sequence=9192,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@597e8670[id=9267,rapidViewId=1151,state=CLOSED,name=Candy Cane Mochi,startDate=2021-12-16T19:52:25.001Z,endDate=2021-12-30T22:52:00.000Z,completeDate=2022-01-06T19:44:14.566Z,sequence=9267,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@5540a235[id=9350,rapidViewId=1151,state=CLOSED,name=Stale Gingerbread House,startDate=2022-01-06T20:09:09.291Z,endDate=2022-01-20T23:09:00.000Z,completeDate=2022-01-20T19:55:49.513Z,sequence=9350,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@51fb0b5[id=9441,rapidViewId=1151,state=CLOSED,name=Egg Wrap,startDate=2022-01-20T20:05:56.996Z,endDate=2022-02-03T23:05:00.000Z,completeDate=2022-02-03T19:40:52.216Z,sequence=9441,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@7c7103bc[id=9544,rapidViewId=1151,state=CLOSED,name=Honey Cardamom Latte,startDate=2022-02-03T20:03:38.315Z,endDate=2022-02-17T23:03:00.000Z,completeDate=2022-02-17T18:20:45.197Z,sequence=9544,goal=]",
			      "com.atlassian.greenhopper.service.sprint.Sprint@46c54791[id=9629,rapidViewId=1151,state=CLOSED,name=Morning Glory Muffin,startDate=2022-02-17T18:43:36.493Z,endDate=2022-03-03T21:43:00.000Z,completeDate=2022-03-03T18:13:07.991Z,sequence=9629,goal=]"
			    ],
		*/
	}
	DefaultRateLimit = 7
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cliapp := &cli.App{
		Name:  "jira-migrator",
		Usage: "migrate tickets server one server to another",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   "config.yaml",
				Usage:   "The configuration file to use.",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "inspect",
				Usage: "Inspect issues",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "host,h",
						Usage: "The host to query. Valid values are \"server\" and \"cloud\"",
						Value: "server",
					},
					&cli.StringFlag{
						Name:     "jql",
						Usage:    "The JQL query string to execute against the configured \"server\" server.",
						Required: true,
					},
				},
				Action: func(c *cli.Context) error {
					config, err := newConfigFromFile(c.String("config"))
					if err != nil {
						return err
					}

					var creds Credentials
					switch host := c.String("host"); host {
					case "server":
						creds = config.Server
					case "cloud":
						creds = config.Cloud
					default:
						return errors.New("invalid host value: " + host)
					}

					client, err := newClient(creds, DefaultRateLimit)
					if err != nil {
						return err
					}

					if err := client.Issue.SearchPages(c.String("jql"), &jira.SearchOptions{
						Expand: "names",
						Fields: DefaultSearchFields,
					}, func(issue jira.Issue) error {
						b, err := json.Marshal(issue)
						if err != nil {
							return err
						}
						fmt.Println(string(b))
						return nil
					}); err != nil {
						return errors.Wrap(err, "unable to search pages")
					}

					return nil
				},
			},
			{
				Name:  "migrate",
				Usage: "Migrate issues server one server to another",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "jql",
						Usage:    "The JQL query string to execute against the configured \"server\" server.",
						Required: true,
					},
					&cli.BoolFlag{
						Name:  "children",
						Usage: "Set if you want to migrate all child issues.",
					},
					&cli.IntFlag{
						Name:  "rate-limit",
						Usage: "Set the api rate limit (max requests per second) to respect.",
						Value: DefaultRateLimit,
					},
				},
				ArgsUsage: "DESTINATION_PROJECT_KEY",
				Action: func(c *cli.Context) error {
					var (
						configFile = c.String("config")
						jql        = c.String("jql")
						children   = c.Bool("children")
						rateLimit  = c.Int("rate-limit")
						projectKey = c.Args().First()

						errg errgroup.Group
					)

					if c.NArg() == 0 {
						return errors.New("must specify a project key")
					}

					config, err := newConfigFromFile(configFile)
					if err != nil {
						return err
					}

					// add flag-based configs
					config.ProjectKey = projectKey
					config.RateLimit = rateLimit

					app, err := NewMigratorApp(*config)
					if err != nil {
						return errors.Wrap(err, "unable to configure app")
					}

					ctx, cancel := context.WithCancel(context.Background())

					if err := app.Server.Issue.SearchPagesWithContext(ctx, jql, &jira.SearchOptions{
						Expand: "names",
						Fields: DefaultSearchFields,
					}, func(issue jira.Issue) error {
						errg.Go(func() (err error) {
							defer func() {
								if err != nil {
									cancel()
								}
							}()

							if err := app.MigrateParents(ctx, &issue); err != nil {
								return errors.Wrap(err, "unable to migrate parents")
							}
							if _, err = app.MigrateIssue(ctx, &issue); err != nil {
								return errors.Wrap(err, "unable to migrate issue")
							}
							if children {
								if err := app.MigrateChildren(ctx, &issue); err != nil {
									return errors.Wrap(err, "unable to migrate children")
								}
							}
							return nil
						})
						return nil
					}); err != nil {
						return errors.Wrap(err, "unable to search pages")
					}

					return errg.Wait()
				},
			},
		},
	}

	err := cliapp.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}

	return
}

type Config struct {
	Server     Credentials `yaml:"server"`
	Cloud      Credentials `yaml:"cloud"`
	ProjectKey string
	RateLimit  int
}

type Credentials struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

func newConfigFromFile(path string) (*Config, error) {
	configFile, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.NewDecoder(configFile).Decode(&config); err != nil {
		return nil, err
	}
	return &config, nil
}

func respErrExit(resp *jira.Response, err error) {
	panic(err)
}

type ErrorResponse struct {
	// {"errorMessages":["This Jira instance is currently under heavy load and is not able to process your request. Try again in a few seconds. If the problem persists, contact Jira support."],"errors":{}}
	ErrorMessages []string    `json:"errorMessages"`
	Errors        interface{} `json:"errors"`
}

func NewProgress() *Progress {
	return &Progress{
		migrating: map[string]bool{},
		migrated:  map[string]string{},
		parents:   map[string]string{},
	}
}

type Progress struct {
	mu        sync.RWMutex
	migrating map[string]bool
	migrated  map[string]string
	parents   map[string]string
}

func (p *Progress) MarkMigrating(server string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.migrating[server] = true
}

func (p *Progress) IsMigrating(server string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.migrating[server]
}

func (p *Progress) MarkMigrated(server, to string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.migrated[server] = to
}

func (p *Progress) MarkMigratedParent(server, toParent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parents[server] = toParent
}

func (p *Progress) MigratedKey(server string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.migrated[server]
}

func (p *Progress) MigratedParentKey(server string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.parents[server]
}

func (p *Progress) IsMigrated(server string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.migrated[server]
	return ok
}

func (p *Progress) IsParentMigrated(server string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.parents[server]
	return ok
}

type BackoffTransport struct {
	limiter *rate.Limiter
}

func (tr *BackoffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return tr.roundTrip(10*time.Second, req)
}

func (tr *BackoffTransport) roundTrip(backoff time.Duration, req *http.Request) (*http.Response, error) {
	if err := tr.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			log.Println(req.URL.Host, req.Method, req.URL.Path, http.StatusText(resp.StatusCode), "Backing off for", backoff)
			secs, err := strconv.ParseInt(retryAfter, 10, 64)
			if err != nil {
				panic(err)
			}
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(time.Duration(secs) * time.Second):
				return tr.RoundTrip(req)
			}
		}
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			log.Println(req.URL.Host, req.Method, req.URL.Path, http.StatusText(resp.StatusCode), "Backing off for", backoff)
			// yyyy-MM-ddTHH:mmZ
			t, err := time.Parse("2006-01-02T15:04Z", reset)
			if err != nil {
				panic(err)
			}
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(time.Until(t)):
				return tr.RoundTrip(req)
			}
		}
	default:
		if resp.StatusCode >= 400 {
			reqDump, _ := httputil.DumpRequest(req, true)
			respDump, _ := httputil.DumpResponse(resp, true)
			fmt.Println(string(reqDump) + string(respDump))

			log.Println(req.URL.Host, req.Method, req.URL.Path, http.StatusText(resp.StatusCode), "Backing off for", backoff)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(backoff):
				// exponential backoff
				return tr.roundTrip(backoff*2, req)
			}
		}
	}

	return resp, nil
}

type MigratorApp struct {
	Server     *jira.Client
	Cloud      *jira.Client
	ProjectKey string
	UserLookup map[string]jira.User
	Progress   *Progress

	// Ensures that we only migrate each issue once without having to implement fancy
	// deduplication logic for this unit of work
	migrateIssueOnce singleflight.Group
	createSprintOnce singleflight.Group
}

func newClient(creds Credentials, rateLimit int) (*jira.Client, error) {
	return jira.NewClient((&jira.BasicAuthTransport{
		Username: creds.Username,
		Password: creds.Password,
		Transport: &BackoffTransport{
			limiter: rate.NewLimiter(rate.Limit(float64(rateLimit)), rateLimit), // N round trips per second
		},
	}).Client(), "https://"+creds.Host)
}

func NewMigratorApp(config Config) (*MigratorApp, error) {
	server, err := newClient(config.Server, config.RateLimit)
	if err != nil {
		return nil, err
	}

	cloud, err := newClient(config.Cloud, config.RateLimit)
	if err != nil {
		return nil, err
	}

	toUsers, err := getUsers(cloud, config.ProjectKey)
	if err != nil {
		return nil, err
	}

	userLookup := map[string]jira.User{}
	for _, user := range toUsers {
		userLookup[user.EmailAddress] = user
	}

	return &MigratorApp{
		Server:     server,
		Cloud:      cloud,
		ProjectKey: config.ProjectKey,
		UserLookup: userLookup,
		Progress:   NewProgress(),
	}, nil
}

func (app *MigratorApp) QueryIssues(ctx context.Context, client *jira.Client, jql string, fields ...string) ([]jira.Issue, error) {
	var issues []jira.Issue
	if err := client.Issue.SearchPagesWithContext(ctx, jql, &jira.SearchOptions{
		Expand: "names",
		Fields: DefaultSearchFields,
	}, func(issue jira.Issue) error {
		issues = append(issues, issue)
		return nil
	}); err != nil {
		return nil, errors.Wrap(err, "unable to search pages")
	}
	return issues, nil
}

// Safe for concurrent use.
func (app *MigratorApp) MigrateChildren(ctx context.Context, parent *jira.Issue) (err error) {
	var children []jira.Issue
	// If its an epic, migrate its issues and any of thier children
	if parent.Fields.Type.Name == "Epic" {
		children, err = app.QueryIssues(ctx, app.Server, `"Epic Link" = `+parent.Key+` ORDER BY key`)
		if err != nil {
			return errors.Wrap(err, "unable to query epic children")
		}
	}
	if len(parent.Fields.Subtasks) > 0 {
		children, err = app.QueryIssues(ctx, app.Server, `parent in ("`+parent.Key+`") ORDER BY key`)
		if err != nil {
			return errors.Wrap(err, "unable to query subtasks")
		}
	}

	// we can migrate each child concurrently
	var errg errgroup.Group

	for _, child := range children {
		child := child // avoid loop closure captures

		// Depends on the parent being migrated already
		app.Progress.MarkMigratedParent(child.Key, app.Progress.MigratedKey(parent.Key))

		errg.Go(func() error {
			_, err := app.MigrateIssue(ctx, &child)
			if err != nil {
				return errors.Wrap(err, "unable to migrate child")
			}
			if err := app.MigrateChildren(ctx, &child); err != nil {
				return errors.Wrap(err, "unable to migrate children's children")
			}
			return nil
		})
	}

	return errg.Wait()
}

func (app *MigratorApp) GetParent(ctx context.Context, issue *jira.Issue) (*jira.Issue, error) {
	var parentKey string
	if parent := issue.Fields.Parent; parent != nil {
		parentKey = parent.Key
	}
	if epicKey := epicKey(issue); epicKey != "" {
		parentKey = epicKey
	}
	if parentKey == "" {
		return nil, nil
	}

	parents, err := app.QueryIssues(ctx, app.Server, `issue = `+parentKey)
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		return nil, nil
	}

	// there can only be one parent
	return &parents[0], nil
}

// func (app *MigratorApp) GetParentRoot(issue *jira.Issue) (*jira.Issue, error) {
// 	// If it's a subtask or has an epic, migrate its parent first.
// 	parent, err := app.GetParent(issue)
// 	if err != nil {
// 		errors.Wrap(err, "unable to get parent")
// 	}
// 	if parent != nil {
// 		return app.GetParentRoot(parent)
// 	}
// 	return issue, nil
// }

// Migrates every parent up to root, but doesn't bother with siblings or cousins
func (app *MigratorApp) MigrateParents(ctx context.Context, issue *jira.Issue) error {
	parent, err := app.GetParent(ctx, issue)
	if err != nil {
		errors.Wrap(err, "unable to get parent")
	}
	if parent != nil {
		if err := app.MigrateParents(ctx, parent); err != nil {
			return err
		}
		migratedParentKey, err := app.MigrateIssue(ctx, parent)
		if err != nil {
			return err
		}
		app.Progress.MarkMigratedParent(issue.Key, migratedParentKey)
	}
	return nil
}

func (app *MigratorApp) MigrateIssue(ctx context.Context, issue *jira.Issue) (string, error) {
	key, err, _ := app.migrateIssueOnce.Do(issue.Key, func() (interface{}, error) {
		fmt.Println("Migrating", issue.Key)
		if key, err := app.migrateIssue(ctx, issue); err != nil {
			fmt.Printf("Error migrating %s: %v\n", issue.Key, err)
			return nil, err
		} else {
			fmt.Println("Successfully migrated", issue.Key, "to", key)
			return key, nil
		}
	})
	return key.(string), err
}

func (app *MigratorApp) migrateIssue(ctx context.Context, issue *jira.Issue) (key string, err error) {
	defer func() {
		if err == nil {
			app.Progress.MarkMigrated(issue.Key, key)
		}
	}()

	// First, check to see if this issue has already been migrated, and skip if so.
	migratedIssues, err := app.QueryIssues(ctx, app.Cloud, `project = `+app.ProjectKey+` AND issue in issuesWithRemoteLinksByGlobalId("`+issue.Key+`") ORDER BY key DESC`)
	if err != nil {
		return "", err
	}
	if len(migratedIssues) > 0 {
		return migratedIssues[0].Key, nil
	}

	newIssue := jira.Issue{
		Fields: &jira.IssueFields{
			Project: jira.Project{
				Key: app.ProjectKey,
			},
			Type: jira.IssueType{
				Name: issue.Fields.Type.Name,
			},
			Summary:     issue.Fields.Summary,
			Description: issue.Fields.Description,
			// TODO: Match priorities
			// Priority: issue.Fields.Priority,
			Labels: issue.Fields.Labels,
		},
	}

	// Jira cloud uses the summary in the heirarchy widget above a ticket. Epic names work better there, trust me.
	// So this prepends the summary to the description field instead.
	if name := epicName(issue); name != "" {
		newIssue.Fields.Summary = name
		newIssue.Fields.Description = issue.Fields.Summary + "\n\n" + issue.Fields.Description
	}

	if reporter := issue.Fields.Reporter; reporter != nil {
		cloudUser, ok := app.UserLookup[reporter.EmailAddress]
		if ok {
			newIssue.Fields.Reporter = &jira.User{AccountID: cloudUser.AccountID}
		}
	}

	if assignee := issue.Fields.Assignee; assignee != nil {
		cloudUser, ok := app.UserLookup[assignee.EmailAddress]
		if ok {
			newIssue.Fields.Assignee = &jira.User{AccountID: cloudUser.AccountID}
		}
	}

	if app.Progress.IsParentMigrated(issue.Key) {
		newIssue.Fields.Parent = &jira.Parent{
			Key: app.Progress.MigratedParentKey(issue.Key),
		}
	}

	// if the issue has a sprint and it's active, then migrate the sprint as well
	if sprint := issue.Fields.Sprint; sprint != nil && sprint.CompleteDate == nil {
		// newSprintID, err, _ := app.createSprintOnce.Do(sprint.Name, func() (interface{}, error) {
		// 	app.Cloud.Board.GetAllSprintsWithOptionsWithContext()
		// 	// 	app.Cloud.Sprint.CreateWithContext(ctx)
		// 	// 	return 0, err
		// })
		// if err != nil {
		// 	return "", errors.Wrapf(err, "Error creating sprint %q", sprint.Name)
		// }
		// issue.Fields.Sprint = &jira.Sprint{
		// 	ID: newSprintID.(int),
		// }
	}

	migrated, _, err := app.Cloud.Issue.CreateWithContext(ctx, &newIssue)
	if err != nil {
		return "", errors.Wrapf(err, "Error creating issue for %s", issue.Key)
	}

	errg := errgroup.Group{}

	errg.Go(func() error {
		if _, _, err := app.Cloud.Issue.AddRemoteLinkWithContext(ctx, migrated.ID, &jira.RemoteLink{
			GlobalID: issue.Key,
			Application: &jira.RemoteLinkApplication{
				Type: "jira.etsycorp.com",
				Name: "Migrated Issue",
			},
			Object: &jira.RemoteLinkObject{
				URL:     "https://jira.etsycorp.com/browse/" + issue.Key,
				Title:   issue.Key,
				Summary: issue.Fields.Summary,
			},
		}); err != nil {
			return errors.Wrap(err, "Error creating remote link")
		}
		return nil
	})

	// Comments can't be set on create. They must be added later
	if issue.Fields.Comments != nil {
		errg.Go(func() error {
			for _, comment := range issue.Fields.Comments.Comments {
				comment := comment
				if _, _, err := app.Cloud.Issue.AddCommentWithContext(ctx, migrated.ID, &jira.Comment{
					Name: comment.Name,
					// It's impossible to set a different author than "self",
					// so just indicate who wrote this originally in the body of the comment.
					Body:       "On " + comment.Created + " " + comment.Author.Name + " wrote:\n\n" + comment.Body,
					Visibility: comment.Visibility,
				}); err != nil {
					return errors.Wrapf(err, "Error adding comment to %s", migrated.Key)
				}
			}
			return nil
		})
	}

	// Attachments can't be set on create, they must be downloaded and posted later
	for _, attachment := range issue.Fields.Attachments {
		attachment := attachment
		errg.Go(func() error {
			req, _ := http.NewRequest("GET", attachment.Content, nil)
			resp, err := app.Server.Do(req.WithContext(ctx), nil)
			if err != nil {
				return errors.Wrapf(err, "Error fetching attachment %q server issue %s", attachment.Filename, issue.Key)
			}
			defer resp.Body.Close()
			if _, _, err := app.Cloud.Issue.PostAttachmentWithContext(ctx, migrated.ID, resp.Body, attachment.Filename); err != nil {
				return errors.Wrapf(err, "Error posting attachment %q to issue %s", attachment.Filename, migrated.Key)
			}
			return nil
		})
	}

	errg.Go(func() error {
		// Transition the ticket to the correct status
		transitions, _, err := app.Cloud.Issue.GetTransitionsWithContext(ctx, migrated.ID)
		if err != nil {
			return errors.Wrapf(err, "Error fetching transitions for %s", migrated.Key)
		}
		for _, transition := range transitions {
			if transition.To.Name == issue.Fields.Status.Name {
				if _, err := app.Cloud.Issue.DoTransitionWithContext(ctx, migrated.ID, transition.ID); err != nil {
					return errors.Wrapf(err, "Error transitioning issue %s to %q", migrated.Key, transition.To.Name)
				}
			}
		}
		return nil
	})

	if err := errg.Wait(); err != nil {
		return "", errors.Wrapf(err, "Error migrating %s", issue.Key)
	}

	return migrated.Key, nil
}

func epicKey(issue *jira.Issue) string {
	if field, ok := issue.Fields.Unknowns["customfield_10620"]; ok {
		if epicKey, ok := field.(string); ok {
			return epicKey
		}
	}
	return ""
}

func epicName(issue *jira.Issue) string {
	if field, ok := issue.Fields.Unknowns["customfield_10621"]; ok {
		if epicName, ok := field.(string); ok {
			return epicName
		}
	}
	return ""
}

func getUsers(cloud *jira.Client, projectKey string) ([]jira.User, error) {
	project, _, err := cloud.Project.Get(projectKey)
	if err != nil {
		return nil, errors.Wrapf(err, "Error retrieving project %s", projectKey)
	}

	var users []jira.User

	for role, resource := range project.Roles {
		req, err := http.NewRequest("GET", resource, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "Error retrieving %s role server %s", role, resource)
		}
		var role jira.Role
		if _, err := cloud.Do(req, &role); err != nil {
			return nil, errors.Wrapf(err, "Error retrieving %s role server %s", role, resource)
		}
		for _, actor := range role.Actors {
			user, _, err := cloud.User.GetByAccountID(actor.ActorUser.AccountID)
			if err != nil {
				return nil, errors.Wrapf(err, "Error retrieving user %q", actor.DisplayName)
			}
			users = append(users, *user)
		}
	}
	return users, nil
}
