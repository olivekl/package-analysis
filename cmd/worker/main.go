package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path"
	"time"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"
	"gocloud.dev/pubsub"
	_ "gocloud.dev/pubsub/gcppubsub"
	_ "gocloud.dev/pubsub/kafkapubsub"

	"github.com/ossf/package-analysis/internal/analysis"
	"github.com/ossf/package-analysis/internal/log"
	"github.com/ossf/package-analysis/internal/pkgecosystem"
	"github.com/ossf/package-analysis/internal/resultstore"
	"github.com/ossf/package-analysis/internal/sandbox"
)

const (
	maxRetries    = 10
	retryInterval = 1
	retryExpRate  = 1.5

	localPkgPathFmt = "/local/%s"
)

func handleMessage(ctx context.Context, msg *pubsub.Message, packagesBucket *blob.Bucket, resultsBucket, imageTag string) error {
	name := msg.Metadata["name"]
	if name == "" {
		log.Warn("name is empty")
		msg.Ack()
		return nil
	}

	ecosystem := msg.Metadata["ecosystem"]
	if ecosystem == "" {
		log.Warn("ecosystem is empty",
			log.Label("name", name))
		msg.Ack()
		return nil
	}

	manager := pkgecosystem.Manager(ecosystem)
	if manager == nil {
		log.Warn("Unsupported pkg manager",
			log.Label("ecosystem", ecosystem),
			log.Label("name", name))
		msg.Ack()
		return nil
	}

	version := msg.Metadata["version"]
	pkgPath := msg.Metadata["package_path"]

	resultsBucketOverride := msg.Metadata["results_bucket_override"]
	if resultsBucketOverride != "" {
		resultsBucket = resultsBucketOverride
	}

	log.Info("Got request",
		log.Label("ecosystem", ecosystem),
		log.Label("name", name),
		log.Label("version", version),
		log.Label("package_path", pkgPath),
		log.Label("results_bucket_override", resultsBucketOverride),
	)

	localPkgPath := ""
	sbOpts := []sandbox.Option{
		sandbox.Tag(imageTag),
	}

	if pkgPath != "" {
		if packagesBucket == nil {
			return errors.New("packages bucket not set")
		}

		// Copy remote package path to temporary file.
		r, err := packagesBucket.NewReader(ctx, pkgPath, nil)
		if err != nil {
			return err
		}
		defer r.Close()

		f, err := ioutil.TempFile("", "")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())

		if _, err := io.Copy(f, r); err != nil {
			return err
		}

		if err := f.Close(); err != nil {
			return err
		}

		localPkgPath = fmt.Sprintf(localPkgPathFmt, path.Base(pkgPath))
		sbOpts = append(sbOpts, sandbox.Volume(f.Name(), localPkgPath))
	}

	var pkg *pkgecosystem.Pkg
	var err error
	if localPkgPath != "" {
		pkg = manager.Local(name, version, localPkgPath)
	} else if version != "" {
		pkg = manager.Package(name, version)
	} else {
		pkg, err = manager.Latest(name)
		if err != nil {
			log.Error("Failed to get latest version",
				log.Label("ecosystem", ecosystem),
				log.Label("name", name),
				"error", err)
			return err
		}
	}

	sb := sandbox.New(manager.Image(), sbOpts...)
	defer sb.Clean()
	results := make(map[string]*analysis.Result)
	finalStatus := analysis.StatusCompleted
	lastPhase := ""
	for _, phase := range manager.DynamicPhases() {
		result, err := analysis.Run(sb, pkg.Command(phase))
		if err != nil {
			log.Error("Analysis run failed",
				log.Label("ecosystem", ecosystem),
				log.Label("name", name),
				log.Label("phase", phase),
				log.Label("version", version),
				"error", err)
			return err
		}
		results[phase] = result
		lastPhase = phase
		finalStatus = result.Status
		// Don't continue processing if the phase did not complete successfully.
		if result.Status != analysis.StatusCompleted {
			break
		}
	}
	// Produce a log message for the final status to help generate metrics.
	switch finalStatus {
	case analysis.StatusCompleted:
		log.Info("Analysis completed sucessfully",
			log.Label("ecosystem", ecosystem),
			log.Label("name", name),
			log.Label("version", version),
			log.Label("last_phase", lastPhase))
	case analysis.StatusErrorAnalysis:
		log.Warn("Analysis error - analysis",
			log.Label("ecosystem", ecosystem),
			log.Label("name", name),
			log.Label("version", version),
			log.Label("last_phase", lastPhase))
	case analysis.StatusErrorTimeout:
		log.Warn("Analysis error - timeout",
			log.Label("ecosystem", ecosystem),
			log.Label("name", name),
			log.Label("version", version),
			log.Label("last_phase", lastPhase))
	case analysis.StatusErrorOther:
		log.Warn("Analysis error - other",
			log.Label("ecosystem", ecosystem),
			log.Label("name", name),
			log.Label("version", version),
			log.Label("last_phase", lastPhase))
	}

	if resultsBucket != "" {
		err := resultstore.New(resultsBucket, resultstore.ConstructPath()).Save(ctx, pkg, results)
		if err != nil {
			return fmt.Errorf("failed to upload to blobstore = %w", err)
		}
	}

	msg.Ack()
	return nil
}

func messageLoop(ctx context.Context, subURL, packagesBucket, resultsBucket, imageTag string) error {
	sub, err := pubsub.OpenSubscription(ctx, subURL)
	if err != nil {
		return err
	}

	var pkgsBkt *blob.Bucket
	if packagesBucket != "" {
		var err error
		pkgsBkt, err = blob.OpenBucket(ctx, packagesBucket)
		if err != nil {
			return err
		}
		defer pkgsBkt.Close()
	}

	log.Info("Listening for messages to process...")
	for {
		msg, err := sub.Receive(ctx)
		if err != nil {
			// All subsequent receive calls will return the same error, so we bail out.
			return fmt.Errorf("error receiving message: %w", err)
		}

		if err := handleMessage(ctx, msg, pkgsBkt, resultsBucket, imageTag); err != nil {
			log.Error("Failed to process message",
				"error", err)
		}
	}
}

func main() {
	retryCount := 0
	ctx := context.Background()
	subURL := os.Getenv("OSSMALWARE_WORKER_SUBSCRIPTION")
	packagesBucket := os.Getenv("OSSF_MALWARE_ANALYSIS_PACKAGES")
	resultsBucket := os.Getenv("OSSF_MALWARE_ANALYSIS_RESULTS")
	imageTag := os.Getenv("OSSF_SANDBOX_IMAGE_TAG")
	log.Initalize(os.Getenv("LOGGER_ENV"))
	sandbox.InitEnv()

	// Log the configuration of the worker at startup so we can observe it.
	log.Info("Starting worker",
		log.Label("subscription", subURL),
		log.Label("package_bucket", packagesBucket),
		log.Label("results_bucket", resultsBucket),
		log.Label("image_tag", imageTag))

	for {
		err := messageLoop(ctx, subURL, packagesBucket, resultsBucket, imageTag)
		if err != nil {
			if retryCount++; retryCount >= maxRetries {
				log.Error("Retries exceeded",
					"error", err,
					"retryCount", retryCount)
				break
			}

			retryDuration := time.Second * time.Duration(retryDelay(retryCount))
			log.Error("Error encountered, retrying",
				"error", err,
				"retryCount", retryCount,
				"waitSeconds", retryDuration.Seconds())
			time.Sleep(retryDuration)
		}
	}
}

func retryDelay(retryCount int) int {
	return int(math.Floor(retryInterval * math.Pow(retryExpRate, float64(retryCount))))
}
