package seeder

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"golang.org/x/net/context"

	"diego-stress-tests/cedar/config"

	"diego-stress-tests/cedar/cli"

	"code.cloudfoundry.org/cflager"
	"code.cloudfoundry.org/lager"
)

type State struct {
	Succeeded bool    `json:"succeeded"`
	StartTime *string `json:"start_time"`
	EndTime   *string `json:"end_time"`
	Duration  int64   `json:"duration_ns"`
}

type AppStateMetrics struct {
	AppName      *string `json:"app_name"`
	AppGuid      *string `json:"app_guid"`
	AppURL       string  `json:"app_url"`
	PushState    *State  `json:"push"`
	StartState   *State  `json:"start"`
	RestartState *State  `json:"start"`
}

const (
	Push    = "push"
	Start   = "start"
	Restart = "restart"
)

type Deployer struct {
	errChan chan error
	config  config.Config

	AppsToPush    []CfApp
	AppsToStart   []CfApp
	AppsToRestart []CfApp
	AppStates     map[string]*AppStateMetrics

	client cli.CFClient
}

func NewDeployer(config config.Config, apps []CfApp, cli cli.CFClient) Deployer {
	p := Deployer{
		errChan:     make(chan error, config.MaxAllowedFailures()),
		AppStates:   make(map[string]*AppStateMetrics),
		config:      config,
		AppsToPush:  apps,
		AppsToStart: apps,
		client:      cli,
	}
	return p
}

func (p *Deployer) PushApps(logger lager.Logger, ctx context.Context, cancel context.CancelFunc) {
	logger = logger.Session("pushing-apps", lager.Data{"max-allowed-failures": p.config.MaxAllowedFailures()})
	logger.Info("started")
	defer logger.Info("complete")

	stateMutex := &sync.Mutex{}
	wg := &sync.WaitGroup{}
	rateLimiter := make(chan struct{}, p.config.MaxInFlight())

	app := p.AppsToPush[0]
	err := p.pushApp(logger, ctx, app, stateMutex)
	if err != nil {
		logger.Error("failed-to-push-initial-app", err)
		cancel()
		return
	}

	for _, app := range p.AppsToPush[1:] {
		app := app
		wg.Add(1)
		go func() {
			rateLimiter <- struct{}{}
			defer func() {
				<-rateLimiter
				wg.Done()
			}()

			select {
			case <-ctx.Done():
				logger.Info("push-cancelled", lager.Data{"app-name": app.AppName()})
				return
			default:
			}

			err := p.pushApp(logger, ctx, app, stateMutex)
			if err != nil {
				logger.Error("failed-pushing-app", err)
				select {
				case p.errChan <- err:
				default:
					logger.Error("exceeded-failure-tolerance", nil)
					cancel()
				}
			}
		}()
	}
	wg.Wait()

	logger.Info("done-pushing-apps", lager.Data{"apps-to-start": len(p.AppsToStart)})
}

func (p *Deployer) pushApp(logger lager.Logger, ctx context.Context, app CfApp, stateMutex *sync.Mutex) error {
	startTime := time.Now()
	pushErr := app.Push(logger, ctx, p.client, p.config.AppPayload(), p.config.Timeout())
	endTime := time.Now()
	succeeded := pushErr == nil

	name := app.AppName()
	guid, err := app.Guid(logger, ctx, p.client, p.config.Timeout())
	if err != nil {
		logger.Error("failed-getting-app-guid", err)
	}

	stateMutex.Lock()
	defer stateMutex.Unlock()

	if succeeded {
		p.AppsToStart = append(p.AppsToStart, app)
	}

	p.AppStates[name] = &AppStateMetrics{
		AppName:    &name,
		AppGuid:    &guid,
		AppURL:     app.AppURL(),
		PushState:  &State{},
		StartState: &State{},
	}
	p.updateReport(Push, name, succeeded, startTime, endTime)

	return pushErr
}

func (p *Deployer) StartApps(ctx context.Context, cancel context.CancelFunc) {
	logger, ok := ctx.Value("logger").(lager.Logger)
	if !ok {
		logger, _ = cflager.New("cedar")
	}
	logger = logger.Session("starting-apps", lager.Data{"max-allowed-failures": p.config.MaxAllowedFailures()})
	logger.Info("started")
	defer logger.Info("completed")

	wg := sync.WaitGroup{}
	rateLimiter := make(chan struct{}, p.config.MaxInFlight())

	for i := 0; i < len(p.AppsToStart); i++ {
		appToStart := p.AppsToStart[i]

		wg.Add(1)

		go func() {
			rateLimiter <- struct{}{}
			defer func() {
				<-rateLimiter
				wg.Done()
			}()

			var err error
			var startTime, endTime time.Time
			select {
			case <-ctx.Done():
				logger.Info("start-cancelled-before-starting-app", lager.Data{"AppName": appToStart.AppName()})
				return
			default:
				startTime = time.Now()
				err = appToStart.Start(logger, ctx, p.client, p.config.SkipVerifyCertificate(), p.config.Timeout())
				endTime = time.Now()
			}

			if err == nil {
				logger.Info("started-app", lager.Data{"AppName": appToStart.AppName()})
			} else {
				logger.Error("failed-starting-app", err, lager.Data{"total-incurred-failures": len(p.errChan) + 1})
				select {
				case p.errChan <- err:
				default:
					logger.Error("failure-tolerance-reached", nil)
					cancel()
				}
			}
			succeeded := err == nil
			p.updateReport(Start, appToStart.AppName(), succeeded, startTime, endTime)

		}()
	}
	wg.Wait()
}

func (p *Deployer) RestartApps(logger lager.Logger, ctx context.Context, cancel context.CancelFunc) {
	logger = logger.Session("Restarting-apps", lager.Data{"max-allowed-failures": p.config.MaxAllowedFailures()})
	logger.Info("started")
	defer logger.Info("complete")

	stateMutex := &sync.Mutex{}
	wg := &sync.WaitGroup{}
	rateLimiter := make(chan struct{}, p.config.MaxInFlight())

	app := p.AppsToPush[0]
	err := p.restartApp(logger, ctx, app, stateMutex)
	if err != nil {
		logger.Error("failed-to-restart-initial-app", err)
		cancel()
		return
	}

	for _, app := range p.AppsToPush[1:] {
		app := app
		wg.Add(1)
		go func() {
			rateLimiter <- struct{}{}
			defer func() {
				<-rateLimiter
				wg.Done()
			}()

			select {
			case <-ctx.Done():
				logger.Info("restart-cancelled", lager.Data{"app-name": app.AppName()})
				return
			default:
			}

			err := p.restartApp(logger, ctx, app, stateMutex)
			if err != nil {
				logger.Error("failed-restarting-app", err)
				select {
				case p.errChan <- err:
				default:
					logger.Error("exceeded-failure-tolerance", nil)
					cancel()
				}
			}
		}()
	}
	wg.Wait()

	logger.Info("done-restarting-apps")
}

func (p *Deployer) restartApp(logger lager.Logger, ctx context.Context, app CfApp, stateMutex *sync.Mutex) error {
	startTime := time.Now()
	restartErr := app.Restart(logger, ctx, p.client, p.config.SkipVerifyCertificate(), p.config.Timeout())
	endTime := time.Now()

	succeeded := restartErr == nil
	name := app.AppName()
	stateMutex.Lock()
	defer stateMutex.Unlock()

	p.AppStates[name] = &AppStateMetrics{
		AppName:      &name,
		AppURL:       app.AppURL(),
		RestartState: &State{},
	}
	p.updateReport(Restart, name, succeeded, startTime, endTime)

	return restartErr
}

type CedarReport struct {
	Succeeded bool              `json:"succeeded"`
	Apps      []AppStateMetrics `json:"apps"`
}

func (p *Deployer) GenerateReport(ctx context.Context, cancel context.CancelFunc) bool {
	logger, ok := ctx.Value("logger").(lager.Logger)
	if !ok {
		logger, _ = cflager.New("cedar")
	}
	logger.Session("generate-reports")
	logger.Info("started")
	defer logger.Info("completed")

	succeeded := true
	select {
	case <-ctx.Done():
		succeeded = false
	default:
	}

	report := CedarReport{
		succeeded,
		[]AppStateMetrics{},
	}

	metricsFile, err := os.OpenFile(p.config.OutputFile(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	defer metricsFile.Close()

	if err != nil {
		logger.Error("error-opening-metrics-output-file", err)
		panic("error-opening-metrics-output-file")
	}

	jsonParser := json.NewEncoder(metricsFile)
	for _, value := range p.AppStates {
		report.Apps = append(report.Apps, *value)
	}
	jsonParser.Encode(report)
	return succeeded
}

func (p *Deployer) updateReport(reportType, name string, succeeded bool, startTime, endTime time.Time) {
	var report *State
	switch reportType {
	case Push:
		report = p.AppStates[name].PushState
	case Start:
		report = p.AppStates[name].StartState
	case Restart:
		report = p.AppStates[name].RestartState
	}
	start := startTime.Format("2006-01-02T15:04:05.000-0700")
	end := endTime.Format("2006-01-02T15:04:05.000-0700")
	duration := endTime.UnixNano() - startTime.UnixNano()

	report.Succeeded = succeeded
	report.StartTime = &start
	report.EndTime = &end
	report.Duration = duration
}
