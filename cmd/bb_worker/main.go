package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"

	re_blobstore "github.com/buildbarn/bb-remote-execution/pkg/blobstore"
	"github.com/buildbarn/bb-remote-execution/pkg/builder"
	"github.com/buildbarn/bb-remote-execution/pkg/cas"
	re_filesystem "github.com/buildbarn/bb-remote-execution/pkg/filesystem"
	re_fuse "github.com/buildbarn/bb-remote-execution/pkg/filesystem/fuse"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_worker"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteworker"
	runner_pb "github.com/buildbarn/bb-remote-execution/pkg/proto/runner"
	"github.com/buildbarn/bb-remote-execution/pkg/runner"
	"github.com/buildbarn/bb-remote-execution/pkg/sync"
	"github.com/buildbarn/bb-storage/pkg/atomic"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/eviction"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/global"
	bb_grpc "github.com/buildbarn/bb-storage/pkg/grpc"
	"github.com/buildbarn/bb-storage/pkg/random"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/empty"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("Usage: bb_worker bb_worker.jsonnet")
	}
	var configuration bb_worker.ApplicationConfiguration
	if err := util.UnmarshalConfigurationFromFile(os.Args[1], &configuration); err != nil {
		log.Fatalf("Failed to read configuration from %s: %s", os.Args[1], err)
	}
	lifecycleState, err := global.ApplyConfiguration(configuration.Global)
	if err != nil {
		log.Fatal("Failed to apply global configuration options: ", err)
	}

	browserURL, err := url.Parse(configuration.BrowserUrl)
	if err != nil {
		log.Fatal("Failed to parse browser URL: ", err)
	}

	// Create connection with scheduler.
	schedulerConnection, err := bb_grpc.DefaultClientFactory.NewClientFromConfiguration(configuration.Scheduler)
	if err != nil {
		log.Fatal("Failed to create scheduler RPC client: ", err)
	}
	schedulerClient := remoteworker.NewOperationQueueClient(schedulerConnection)

	// Location for storing temporary file objects. This is
	// currently only used by FUSE to store output files of build
	// actions. Going forward, this may be used to store core dumps
	// generated by build actions as well.
	filePool, err := re_filesystem.NewFilePoolFromConfiguration(configuration.FilePool)
	if err != nil {
		log.Fatal("Failed to create file pool: ", err)
	}

	// Storage access.
	globalContentAddressableStorage, actionCache, err := blobstore_configuration.NewCASAndACBlobAccessFromConfiguration(
		configuration.Blobstore,
		bb_grpc.DefaultClientFactory,
		int(configuration.MaximumMessageSizeBytes))
	if err != nil {
		log.Fatal(err)
	}
	globalContentAddressableStorage = re_blobstore.NewExistencePreconditionBlobAccess(globalContentAddressableStorage)

	// Cached read access for directory objects stored in the
	// Content Addressable Storage. All workers make use of the same
	// cache, to increase the hit rate.
	directoryFetcher := cas.NewCachingDirectoryFetcher(
		cas.NewBlobAccessDirectoryFetcher(
			globalContentAddressableStorage,
			int(configuration.MaximumMessageSizeBytes)),
		digest.KeyWithoutInstance,
		int(configuration.MaximumMemoryCachedDirectories),
		eviction.NewMetricsSet(eviction.NewRRSet(), "CachingDirectoryFetcher"))

	instanceName, err := digest.NewInstanceName(configuration.InstanceName)
	if err != nil {
		log.Fatalf("Invalid instance name %#v: %s", configuration.InstanceName, err)
	}

	if len(configuration.BuildDirectories) == 0 {
		log.Fatal("Cannot start worker without any build directories")
	}
	for _, buildDirectoryConfiguration := range configuration.BuildDirectories {
		var fuseBuildDirectory re_fuse.PrepopulatedDirectory
		var naiveBuildDirectory filesystem.DirectoryCloser
		var fileFetcher cas.FileFetcher
		uploadBatchSize := blobstore.RecommendedFindMissingDigestsCount
		switch backend := buildDirectoryConfiguration.Backend.(type) {
		case *bb_worker.BuildDirectoryConfiguration_Fuse:
			rootInodeNumber := random.FastThreadSafeGenerator.Uint64()
			var serverCallbacks re_fuse.SimpleRawFileSystemServerCallbacks
			fuseBuildDirectory = re_fuse.NewInMemoryPrepopulatedDirectory(
				re_fuse.NewPoolBackedFileAllocator(
					re_filesystem.EmptyFilePool,
					util.DefaultErrorLogger,
					random.FastThreadSafeGenerator),
				util.DefaultErrorLogger,
				rootInodeNumber,
				random.FastThreadSafeGenerator,
				serverCallbacks.EntryNotify)
			if err := re_fuse.NewMountFromConfiguration(
				backend.Fuse,
				fuseBuildDirectory,
				rootInodeNumber,
				&serverCallbacks,
				"bb_worker"); err != nil {
				log.Fatal("Failed to mount build directory: ", err)
			}
		case *bb_worker.BuildDirectoryConfiguration_Native:
			// Directory where actual builds take place.
			nativeConfiguration := backend.Native
			naiveBuildDirectory, err = filesystem.NewLocalDirectory(nativeConfiguration.BuildDirectoryPath)
			if err != nil {
				log.Fatalf("Failed to open build directory %v: %s", nativeConfiguration.BuildDirectoryPath, err)
			}

			// Create a cache directory that holds input
			// files that can be hardlinked into build
			// directory.
			//
			// TODO: Have a single process-wide hardlinking
			// cache even if multiple build directories are
			// used. This increases cache hit rate.
			cacheDirectory, err := filesystem.NewLocalDirectory(nativeConfiguration.CacheDirectoryPath)
			if err != nil {
				log.Fatal("Failed to open cache directory: ", err)
			}
			if err := cacheDirectory.RemoveAllChildren(); err != nil {
				log.Fatal("Failed to clear cache directory: ", err)
			}
			evictionSet, err := eviction.NewSetFromConfiguration(nativeConfiguration.CacheReplacementPolicy)
			if err != nil {
				log.Fatal("Failed to create eviction set for cache directory: ", err)
			}
			fileFetcher = cas.NewHardlinkingFileFetcher(
				cas.NewBlobAccessFileFetcher(globalContentAddressableStorage),
				cacheDirectory,
				int(nativeConfiguration.MaximumCacheFileCount),
				nativeConfiguration.MaximumCacheSizeBytes,
				eviction.NewMetricsSet(evictionSet, "HardlinkingFileFetcher"))

			// Using a native file system requires us to
			// hold on to file descriptors while uploading
			// outputs. Limit the batch size to ensure that
			// we don't exhaust file descriptors.
			uploadBatchSize = 100
		default:
			log.Fatal("No build directory specified")
		}

		var buildDirectoryInitializer sync.Initializer
		var sharedBuildDirectoryNextParallelActionID atomic.Uint64
		if len(buildDirectoryConfiguration.Runners) == 0 {
			log.Fatal("Cannot start worker without any runners")
		}
		for _, runnerConfiguration := range buildDirectoryConfiguration.Runners {
			if runnerConfiguration.Concurrency < 1 {
				log.Fatal("Runner concurrency must be positive")
			}
			concurrencyLength := len(strconv.FormatUint(runnerConfiguration.Concurrency-1, 10))

			defaultExecutionTimeout, err := ptypes.Duration(runnerConfiguration.DefaultExecutionTimeout)
			if err != nil {
				log.Fatal("Failed to parse default execution timeout")
			}
			maximumExecutionTimeout, err := ptypes.Duration(runnerConfiguration.MaximumExecutionTimeout)
			if err != nil {
				log.Fatal("Failed to parse maximum execution timeout")
			}

			// Obtain raw device numbers of character
			// devices that need to be available within the
			// input root.
			inputRootCharacterDevices, err := getInputRootCharacterDevices(
				runnerConfiguration.InputRootCharacterDeviceNodes)
			if err != nil {
				log.Fatal(err)
			}

			// Execute commands using a separate runner process. Due to the
			// interaction between threads, forking and execve() returning
			// ETXTBSY, concurrent execution of build actions can only be
			// used in combination with a runner process. Having a separate
			// runner process also makes it possible to apply privilege
			// separation.
			runnerConnection, err := bb_grpc.DefaultClientFactory.NewClientFromConfiguration(runnerConfiguration.Endpoint)
			if err != nil {
				log.Fatal("Failed to create runner RPC client: ", err)
			}
			runnerClient := runner_pb.NewRunnerClient(runnerConnection)

			for threadID := uint64(0); threadID < runnerConfiguration.Concurrency; threadID++ {
				go func(runnerConfiguration *bb_worker.RunnerConfiguration, threadID uint64) {
					// Per-worker separate writer of the Content
					// Addressable Storage that batches writes after
					// completing the build action.
					contentAddressableStorageWriter, contentAddressableStorageFlusher := re_blobstore.NewBatchedStoreBlobAccess(
						globalContentAddressableStorage,
						digest.KeyWithoutInstance,
						uploadBatchSize)
					contentAddressableStorageWriter = blobstore.NewMetricsBlobAccess(
						contentAddressableStorageWriter,
						clock.SystemClock,
						"cas_batched_store")

					// When FUSE is enabled, we can lazily load the
					// input root, as opposed to explicitly
					// instantiating it before every build.
					var buildDirectory builder.BuildDirectory
					if fuseBuildDirectory != nil {
						buildDirectory = builder.NewFUSEBuildDirectory(
							fuseBuildDirectory,
							directoryFetcher,
							contentAddressableStorageWriter,
							re_fuse.NewRandomInodeNumberTree())
					} else {
						buildDirectory = builder.NewNaiveBuildDirectory(
							naiveBuildDirectory,
							directoryFetcher,
							fileFetcher,
							contentAddressableStorageWriter)
					}

					// Create a per-action subdirectory in
					// the build directory named after the
					// action digest, so that multiple
					// actions may be run concurrently.
					//
					// Also clean the build directory every
					// time when going from fully idle to
					// executing one action.
					buildDirectoryCreator := builder.NewSharedBuildDirectoryCreator(
						builder.NewCleanBuildDirectoryCreator(
							builder.NewRootBuildDirectoryCreator(buildDirectory),
							&buildDirectoryInitializer),
						&sharedBuildDirectoryNextParallelActionID)

					workerID := map[string]string{}
					if runnerConfiguration.Concurrency > 1 {
						workerID["thread"] = fmt.Sprintf("%0*d", concurrencyLength, threadID)
					}
					for k, v := range runnerConfiguration.WorkerId {
						workerID[k] = v
					}
					workerName, err := json.Marshal(workerID)
					if err != nil {
						log.Fatal("Failed to marshal worker ID: ", err)
					}

					buildExecutor := builder.NewLoggingBuildExecutor(
						builder.NewCachingBuildExecutor(
							builder.NewMetricsBuildExecutor(
								builder.NewFilePoolStatsBuildExecutor(
									builder.NewTimestampedBuildExecutor(
										builder.NewStorageFlushingBuildExecutor(
											builder.NewLocalBuildExecutor(
												contentAddressableStorageWriter,
												buildDirectoryCreator,
												runner.NewRemoteRunner(runnerConnection),
												clock.SystemClock,
												defaultExecutionTimeout,
												maximumExecutionTimeout,
												inputRootCharacterDevices,
												int(configuration.MaximumMessageSizeBytes)),
											contentAddressableStorageFlusher),
										clock.SystemClock,
										string(workerName)))),
							globalContentAddressableStorage,
							actionCache,
							browserURL),
						browserURL)

					buildClient := builder.NewBuildClient(
						schedulerClient,
						buildExecutor,
						re_filesystem.NewQuotaEnforcingFilePool(
							filePool,
							runnerConfiguration.MaximumFilePoolFileCount,
							runnerConfiguration.MaximumFilePoolSizeBytes),
						clock.SystemClock,
						workerID,
						instanceName,
						runnerConfiguration.Platform)

					// Contact the scheduler to ask for
					// work. Hold off in case we're idle and
					// our runner process isn't ready.
					//
					// TODO: Add a signal handler here, so
					// that we can terminate workers without
					// interrupting work.
					for {
						if !buildClient.InExecutingState() {
							for {
								_, err := runnerClient.CheckReadiness(context.Background(), &empty.Empty{})
								if err == nil {
									break
								}
								log.Printf("Runner for worker %s is not ready: %s", workerName, err)
								time.Sleep(3 * time.Second)
							}
						}

						if err := buildClient.Run(); err != nil {
							log.Printf("Worker %s: %s", workerName, err)
							time.Sleep(3 * time.Second)
						}
					}
				}(runnerConfiguration, threadID)
			}
		}
	}

	lifecycleState.MarkReadyAndWait()
}
