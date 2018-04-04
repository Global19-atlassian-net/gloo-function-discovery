package eventloop

import (
	"time"

	"github.com/pkg/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/solo-io/gloo-api/pkg/api/types/v1"
	"github.com/solo-io/gloo-function-discovery/internal/detector"
	"github.com/solo-io/gloo-function-discovery/internal/grpc"
	"github.com/solo-io/gloo-function-discovery/internal/nats-streaming"
	"github.com/solo-io/gloo-function-discovery/internal/options"
	"github.com/solo-io/gloo-function-discovery/internal/swagger"
	"github.com/solo-io/gloo-function-discovery/internal/updater"
	"github.com/solo-io/gloo-function-discovery/internal/upstreamwatcher"
	"github.com/solo-io/gloo-function-discovery/pkg/resolver"
	"github.com/solo-io/gloo-storage"
	"github.com/solo-io/gloo-storage/crd"
	"github.com/solo-io/gloo-storage/dependencies"
	filestorage "github.com/solo-io/gloo-storage/dependencies/file"
	"github.com/solo-io/gloo-storage/dependencies/kube"
	"github.com/solo-io/gloo-storage/file"
	"github.com/solo-io/gloo/pkg/bootstrap"
	"github.com/solo-io/gloo/pkg/log"
	"github.com/solo-io/gloo/pkg/secretwatcher"
	filesecrets "github.com/solo-io/gloo/pkg/secretwatcher/file"
	kubesecrets "github.com/solo-io/gloo/pkg/secretwatcher/kube"
	"github.com/solo-io/gloo/pkg/secretwatcher/vault"
)

const (
	maxThreadsPerUpstream = 25
)

type workItem struct {
	upstream *v1.Upstream
	secrets  secretwatcher.SecretMap
}

func Run(opts bootstrap.Options, discoveryOpts options.DiscoveryOptions, stop <-chan struct{}, errs chan error) error {
	store, err := createStorageClient(opts)
	if err != nil {
		return errors.Wrap(err, "failed to create config store client")
	}

	upstreams, err := upstreamwatcher.WatchUpstreams(store, stop, errs)
	if err != nil {
		return errors.Wrap(err, "failed to start monitoring upstreams")
	}

	secretWatcher, err := setupSecretWatcher(opts, stop)
	if err != nil {
		return errors.Wrap(err, "failed to set up secret watcher")
	}

	resolve := createResolver(opts)

	var detectors []detector.Interface
	if discoveryOpts.AutoDiscoverNATS {
		//TODO: support cluster ids
		detectors = append(detectors, nats.NewNatsDetector(""))
	}
	if discoveryOpts.AutoDiscoverSwagger {
		detectors = append(detectors, swagger.NewSwaggerDetector(discoveryOpts.SwaggerUrisToTry))
	}
	if discoveryOpts.AutoDiscoverGRPC {
		files, err := createFileStorageClient(opts)
		if err != nil {
			return errors.Wrap(err, "creating file storage client")
		}
		detectors = append(detectors, grpc.NewGRPCDetector(files))
	}

	marker := detector.NewMarker(detectors, resolve)

	var cache struct {
		secrets   secretwatcher.SecretMap
		upstreams []*v1.Upstream
	}

	workQueues := make(map[string]chan *workItem)

	updateUpstream := func(us *v1.Upstream, secrets secretwatcher.SecretMap) {
		log.Debugf("attempting update for %v", us.Name)
		if err := updater.UpdateServiceInfo(store, us.Name, marker); err != nil {
			errs <- errors.Wrapf(err, "updating upstream %v", us.Name)
		}
		if err := updater.UpdateFunctions(store, us.Name, secrets); err != nil {
			errs <- errors.Wrapf(err, "updating upstream %v", us.Name)
		}
	}

	update := func() {
		var usNames []string
		for _, us := range cache.upstreams {
			usNames = append(usNames, us.Name)
		}
		log.Debugf("beginning update for %v upstreams: %v", usNames, len(cache.upstreams))

		// clean queues for upstreams that have been deleted
		for usName := range workQueues {
			var upstreamFound bool
			for _, us := range cache.upstreams {
				if usName == us.Name {
					upstreamFound = true
					break
				}
			}
			if !upstreamFound {
				close(workQueues[usName])
				delete(workQueues, usName)
			}
		}

		// updating secret refs can happen async
		// if new secrets come in, it will trigger a new update
		go func(upstreams []*v1.Upstream) {
			// update secret refs on secret watcher
			refs := updater.GetSecretRefsToWatch(upstreams)
			secretWatcher.TrackSecrets(refs)
		}(cache.upstreams)

		for _, us := range cache.upstreams {
			_, ok := workQueues[us.Name]
			if !ok {
				workQueues[us.Name] = make(chan *workItem, maxThreadsPerUpstream)
				// start worker thread for this upstream
				go func(workQueues map[string]chan *workItem, usName string) {
					log.Debugf("starting goroutine for %s", usName)
					// allow upstream time to start up
					time.Sleep(time.Second * 2)
					for work := range workQueues[usName] {
						updateUpstream(work.upstream, work.secrets)
					}
					log.Debugf("exiting goroutine for %s", usName)
				}(workQueues, us.Name)
			}
			workQueues[us.Name] <- &workItem{upstream: us, secrets: cache.secrets}
		}
	}

	ticker := time.NewTicker(opts.ConfigWatcherOptions.SyncFrequency)
	defer ticker.Stop()
	for {
		select {
		case cache.secrets = <-secretWatcher.Secrets():
			update()
		case cache.upstreams = <-upstreams:
			update()
		case <-ticker.C:
			update()
		case err := <-secretWatcher.Error():
			errs <- err
		case <-stop:
			return nil
		}
	}
}

func createStorageClient(opts bootstrap.Options) (storage.Interface, error) {
	switch opts.ConfigWatcherOptions.Type {
	case bootstrap.WatcherTypeFile:
		dir := opts.FileOptions.ConfigDir
		if dir == "" {
			return nil, errors.New("must provide directory for file config watcher")
		}
		client, err := file.NewStorage(dir, opts.ConfigWatcherOptions.SyncFrequency)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start file config watcher for directory %v", dir)
		}
		return client, nil
	case bootstrap.WatcherTypeKube:
		cfg, err := clientcmd.BuildConfigFromFlags(opts.KubeOptions.MasterURL, opts.KubeOptions.KubeConfig)
		if err != nil {
			return nil, errors.Wrap(err, "building kube restclient")
		}
		cfgWatcher, err := crd.NewStorage(cfg, opts.KubeOptions.Namespace, opts.ConfigWatcherOptions.SyncFrequency)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start kube config watcher with config %#v", opts.KubeOptions)
		}
		return cfgWatcher, nil
	}
	return nil, errors.Errorf("unknown or unspecified config watcher type: %v", opts.ConfigWatcherOptions.Type)
}

func createFileStorageClient(opts bootstrap.Options) (dependencies.FileStorage, error) {
	switch opts.FileWatcherOptions.Type {
	case bootstrap.WatcherTypeFile:
		dir := opts.FileOptions.FilesDir
		if dir == "" {
			return nil, errors.New("must provide directory for file file watcher")
		}
		client, err := filestorage.NewFileStorage(dir, opts.FileWatcherOptions.SyncFrequency)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start file based file watcher for directory %v", dir)
		}
		return client, nil
	case bootstrap.WatcherTypeKube:
		cfg, err := clientcmd.BuildConfigFromFlags(opts.KubeOptions.MasterURL, opts.KubeOptions.KubeConfig)
		if err != nil {
			return nil, errors.Wrap(err, "building kube restclient")
		}
		cfgWatcher, err := kube.NewFileStorage(cfg, opts.KubeOptions.Namespace, opts.FileWatcherOptions.SyncFrequency)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start kube file watcher with config %#v", opts.KubeOptions)
		}
		return cfgWatcher, nil
	}
	return nil, errors.Errorf("unknown or unspecified file watcher type: %v", opts.FileWatcherOptions.Type)
}

func createResolver(opts bootstrap.Options) *resolver.Resolver {
	kube, err := func() (kubernetes.Interface, error) {
		cfg, err := clientcmd.BuildConfigFromFlags(opts.KubeOptions.MasterURL, opts.KubeOptions.KubeConfig)
		if err != nil {
			return nil, err
		}
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		return kube, nil
	}()
	if err != nil {
		log.Warnf("create kube client failed: %v. swagger services running in kubernetes will not be discovered by function discovery")
	}
	return &resolver.Resolver{Kube: kube}
}

func setupSecretWatcher(opts bootstrap.Options, stop <-chan struct{}) (secretwatcher.Interface, error) {
	switch opts.SecretWatcherOptions.Type {
	case bootstrap.WatcherTypeFile:
		secretWatcher, err := filesecrets.NewSecretWatcher(opts.FileOptions.SecretDir, opts.SecretWatcherOptions.SyncFrequency)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start file secret watcher with config %#v", opts.KubeOptions)
		}
		return secretWatcher, nil
	case bootstrap.WatcherTypeKube:
		secretWatcher, err := kubesecrets.NewSecretWatcher(opts.KubeOptions.MasterURL, opts.KubeOptions.KubeConfig, opts.SecretWatcherOptions.SyncFrequency, stop)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start kube secret watcher with config %#v", opts.KubeOptions)
		}
		return secretWatcher, nil
	case bootstrap.WatcherTypeVault:
		secretWatcher, err := vault.NewVaultSecretWatcher(opts.SecretWatcherOptions.SyncFrequency, opts.VaultOptions.Retries, opts.VaultOptions.VaultAddr, opts.VaultOptions.AuthToken, stop)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start vault secret watcher with config %#v", opts.VaultOptions)
		}
		return secretWatcher, nil
	}
	return nil, errors.Errorf("unknown or unspecified secret watcher type: %v", opts.SecretWatcherOptions.Type)
}
