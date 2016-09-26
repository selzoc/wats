package wats

import (
	"regexp"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gbytes"
	. "github.com/onsi/gomega/gexec"

	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/cloudfoundry-incubator/cf-test-helpers/helpers"
)

type AppUsageEvent struct {
	Entity struct {
		AppName       string `json:"app_name"`
		State         string `json:"state"`
		BuildpackName string `json:"buildpack_name"`
		BuildpackGuid string `json:"buildpack_guid"`
	} `json:"entity"`
}

type AppUsageEvents struct {
	Resources []AppUsageEvent `struct:"resources"`
}

func lastAppUsageEvent(appName string, state string) (bool, AppUsageEvent) {
	var response AppUsageEvents
	cf.AsUser(context.AdminUserContext(), DEFAULT_TIMEOUT, func() {
		cf.ApiRequest("GET", "/v2/app_usage_events?order-direction=desc&page=1&results-per-page=150", &response, DEFAULT_TIMEOUT)
	})

	for _, event := range response.Resources {
		if event.Entity.AppName == appName && event.Entity.State == state {
			return true, event
		}
	}

	return false, AppUsageEvent{}
}

var _ = Describe("Application Lifecycle", func() {
	apps := func() *Session {
		return cf.Cf("apps").Wait()
	}

	reportedIDs := func(instances int) map[string]bool {
		timer := time.NewTimer(time.Second * 120)
		defer timer.Stop()
		run := true
		go func() {
			<-timer.C
			run = false
		}()

		seenIDs := map[string]bool{}
		for len(seenIDs) != instances && run == true {
			seenIDs[helpers.CurlApp(appName, "/id")] = true
			time.Sleep(time.Second)
		}

		return seenIDs
	}

	differentIDsFrom := func(idsBefore map[string]bool) []string {
		differentIDs := []string{}

		for id, _ := range reportedIDs(len(idsBefore)) {
			if !idsBefore[id] {
				differentIDs = append(differentIDs, id)
			}
		}

		return differentIDs
	}

	Describe("An app staged on Diego and running on Diego", func() {
		It("exercises the app through its lifecycle", func() {
			By("pushing it", func() {
				Eventually(pushNora(appName), CF_PUSH_TIMEOUT).Should(Succeed())
			})

			By("staging and running it on Diego", func() {
				enableDiego(appName)
				Eventually(runCf("start", appName), CF_PUSH_TIMEOUT).Should(Succeed())
			})

			By("generates an app usage 'started' event", func() {
				found, _ := lastAppUsageEvent(appName, "STARTED")
				Expect(found).To(BeTrue())
			})

			By("verifying it's up", func() {
				Eventually(helpers.CurlingAppRoot(appName)).Should(ContainSubstring("hello i am nora"))
			})

			By("verifying reported disk/memory usage", func() {
				// #0   running   2015-06-10 02:22:39 PM   0.0%   48.7M of 2G   14M of 1G
				var metrics = regexp.MustCompile(`running.*(?:[\d\.]+)%\s+([\d\.]+)[MG]? of (?:[\d\.]+)[MG]\s+([\d\.]+)[MG]? of (?:[\d\.]+)[MG]`)
				memdisk := func() (float64, float64) {
					output, err := runCfWithOutput("app", appName)
					Expect(err).ToNot(HaveOccurred())
					arr := metrics.FindStringSubmatch(string(output.Contents()))
					mem, err := strconv.ParseFloat(arr[1], 64)
					Expect(err).ToNot(HaveOccurred())
					disk, err := strconv.ParseFloat(arr[2], 64)
					Expect(err).ToNot(HaveOccurred())
					return mem, disk
				}
				Eventually(func() float64 { m, _ := memdisk(); return m }).Should(BeNumerically(">", 0.0))
				Eventually(func() float64 { _, d := memdisk(); return d }()).Should(BeNumerically(">", 0.0))
			})

			By("makes system environment variables available", func() {
				Eventually(func() string {
					return helpers.CurlApp(appName, "/env")
				}, DEFAULT_TIMEOUT).Should(ContainSubstring(`"INSTANCE_GUID"`))
			})

			By("stopping it", func() {
				Eventually(runCf("stop", appName)).Should(Succeed())
				Eventually(helpers.CurlingAppRoot(appName)).Should(ContainSubstring("404"))
			})

			By("setting an environment variable", func() {
				Eventually(runCf("set-env", appName, "FOO", "bar")).Should(Succeed())
			})

			By("starting it", func() {
				Eventually(runCf("start", appName), CF_PUSH_TIMEOUT).Should(Succeed())
				Eventually(helpers.CurlingAppRoot(appName)).Should(ContainSubstring("hello i am nora"))
			})

			By("checking custom env variables are available", func() {
				Eventually(func() string {
					return helpers.CurlAppWithTimeout(appName, "/env/FOO", 30*time.Second)
				}).Should(ContainSubstring("bar"))
			})

			By("scaling it", func() {
				Eventually(runCf("scale", appName, "-i", "2")).Should(Succeed())
				Eventually(apps).Should(Say("2/2"))
				Expect(cf.Cf("app", appName).Wait()).ToNot(Say("insufficient resources"))
			})

			By("restarting an instance", func() {
				idsBefore := reportedIDs(2)
				Expect(len(idsBefore)).To(Equal(2))
				Eventually(cf.Cf("restart-app-instance", appName, "1")).Should(Exit(0))
				Eventually(func() []string {
					return differentIDsFrom(idsBefore)
				}, time.Second*120).Should(HaveLen(1))
			})

			By("updating, is reflected through another push", func() {
				Eventually(helpers.CurlingAppRoot(appName)).Should(ContainSubstring("hello i am nora"))

				// We don't have to set the stack, since that's already done for the app
				// in the BeforeEach and diego keeps that state across multiple pushes
				Expect(cf.Cf(
					"push", appName,
					"-p", "../../assets/webapp",
				).Wait(CF_PUSH_TIMEOUT)).To(Exit(0))

				Eventually(helpers.CurlingAppRoot(appName)).Should(ContainSubstring("hi i am a standalone webapp"))
			})

			By("removing it", func() {
				Expect(cf.Cf("delete", appName, "-f").Wait(DEFAULT_TIMEOUT)).To(Exit(0))
				app := cf.Cf("app", appName).Wait(DEFAULT_TIMEOUT)
				Expect(app).To(Exit(1))
				Expect(app).To(Say("not found"))

				Eventually(func() string {
					return helpers.CurlAppRoot(appName)
				}, DEFAULT_TIMEOUT).Should(ContainSubstring("404"))

				found, _ := lastAppUsageEvent(appName, "STOPPED")
				Expect(found).To(BeTrue())
			})
		})
	})
})
