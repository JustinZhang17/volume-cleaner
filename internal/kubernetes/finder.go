package kubernetes

import (
	// standard packages
	"context"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	/* Unfortunate that a lot of the kubernetes packages require renaming because
	they do not abide by good package name conventions as per https://go.dev/blog/package-names
	*/

	// external packages
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	// internal packages
	structInternal "volume-cleaner/internal/structure"
	utilsInternal "volume-cleaner/internal/utils"
)

// main scheduler logic to find stale pvcs, send emails and delete them

func FindStale(kube kubernetes.Interface, cfg structInternal.SchedulerConfig) {
	// One http client is created for emailing users
	client := &http.Client{Timeout: 10 * time.Second}

	errCount := 0
	deleteCount := 0
	emailCount := 0

	// Sort in descending order
	sort.Slice(cfg.NotifTimes, func(i, j int) bool {
		return cfg.NotifTimes[i] > cfg.NotifTimes[j]
	})

	// iterate through all pvcs in configured namespace(s)
	for _, pvc := range PvcList(kube, cfg.Namespace) {
		log.Printf("Found pvc %s from namespace %s", pvc.Name, pvc.Namespace)

		// check if label exists (meaning pvc is unattached)
		// if pvc is attached to a sts, it would've had its label removed by the controller

		timestamp, ok := pvc.Labels[cfg.TimeLabel]
		if !ok {
			log.Printf("Label %s not found on PVC %s. Skipping.", cfg.TimeLabel, pvc.Name)
			continue
		}

		// check if pvc should be deleted
		stale, staleError := IsStale(timestamp, cfg.TimeFormat, cfg.GracePeriod)
		if staleError != nil {
			log.Printf("Could not parse time: %s", staleError)
			errCount++
			continue
		}

		// stale means grace period has passed, can be deleted
		if stale {
			if cfg.DryRun {
				log.Printf("DRY RUN: delete pvc %s", pvc.Name)
				deleteCount++
				continue
			}

			err := kube.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(context.TODO(), pvc.Name, metav1.DeleteOptions{})
			if err != nil {
				log.Printf("Error deleting pvc %s: %s", pvc.Name, err)
				errCount++
				continue
			}

			log.Print("PVC successfully deleted.")
			deleteCount++

		} else {
			// not stale yet, handle email logic here

			log.Print("Grace period not passed.")

			notifCount, ok := pvc.Labels[cfg.NotifLabel]
			if !ok {
				log.Printf("Label %s not found on PVC %s", cfg.NotifLabel, pvc.Name)
				errCount++
				continue
			}

			currNotif, countErr := strconv.Atoi(notifCount)
			if countErr != nil {
				log.Printf("Error converting notification-count %s: %v", notifCount, countErr)
				errCount++
				continue
			}

			shouldSend, mailError := ShouldSendMail(timestamp, currNotif, cfg)
			if mailError != nil {
				log.Printf("Could not parse time: %s", mailError)
				errCount++
				continue
			}

			if shouldSend {
				if cfg.DryRun {
					log.Print("DRY RUN: email user")
					emailCount++
					continue
				}

				// personal consists of details passed into the email template as variables while email is
				// the email address that is consistent regardless of the template

				email, personal := utilsInternal.EmailDetails(kube, pvc, cfg.GracePeriod)

				err := utilsInternal.SendNotif(client, cfg.EmailCfg, email, personal)
				if err != nil {
					log.Printf("Error: Unable to send an email to %s at %s", personal.Name, email)
					errCount++
					continue
				}

				// Update Email Count
				emailCount++

				// Increment notification count by 1
				newNotifCount := strconv.Itoa(currNotif + 1)
				SetPvcLabel(kube, cfg.NotifLabel, newNotifCount, pvc.Namespace, pvc.Name)

			}
		}
	}

	log.Printf("Job errors %d", errCount)
	log.Printf("Emails sent: %d", emailCount)
	log.Printf("Pvcs deleted: %d", deleteCount)
}

// determines if the grace period is greater than a given timestamp

func IsStale(timestamp string, format string, gracePeriod int) (bool, error) {
	timeObj, err := time.Parse(format, timestamp)
	if err != nil {
		return false, err
	}

	// difference in days
	diff := time.Since(timeObj).Hours() / 24

	log.Printf("Parsed timestamp: %f days.", diff)

	stale := int(diff) > gracePeriod

	log.Printf("int(diff) > cfg.GracePeriod: %v > %v == %v", int(diff), gracePeriod, stale)

	return stale, nil
}

// checks email times and determines if this pvc's owner should be emailed

func ShouldSendMail(timestamp string, currNotif int, cfg structInternal.SchedulerConfig) (bool, error) {
	log.Print("Checking email times....")

	timeObj, err := time.Parse(cfg.TimeFormat, timestamp)
	if err != nil {
		return false, err
	}
	daysLeft := cfg.GracePeriod - int(math.Floor(time.Since(timeObj).Hours()/24))

	log.Printf("Days left until deletion: %d", daysLeft)

	// this logic ensures that emails are eventually sent even if the
	// scheduler is down and misses a few days

	if currNotif < len(cfg.NotifTimes) && cfg.NotifTimes[currNotif] >= daysLeft {
		log.Printf("Email time: %v", cfg.NotifTimes[currNotif])
		return true, nil
	}

	return false, nil
}
