package main

import (
	"bytes"
	"encoding/json"
        "errors"
	"flag"
	"fmt"
        fthealth "github.com/Financial-Times/go-fthealth"
	"github.com/dchest/uniuri"
	"github.com/golang/go/src/pkg/encoding/base64"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

type syntheticPublication struct {
	postEndpoint      string
	s3Endpoint        string
	uuid              string
	latestImage       chan postedData
	latestPublication chan publication

	mutex   *sync.Mutex
	history []publication
}

type postedData struct {
	time time.Time
	//base64 encoded string representation of the generated image
	img string
}

type publication struct {
	time      time.Time
	succeeded bool
	errorMsg  string
}

var postHost = flag.String("postHost", "cms-notifier-pr-uk-int.svc.ft.com", "publish entrypoint host name (e.g. address of cms-notifier in UCS)")
var s3Host = flag.String("s3Host", "com.ft.imagepublish.int.s3.amazonaws.com", "saved image endpoint host name (e.g. address of the s3 service)")
var tick = flag.Bool("tick", true, "true, if this service should periodially generate and post content to the post endpoint")

//fixed
var uuid = "c94a3a57-3c99-423c-a6bd-ed8c4c10a3c3"

func main() {
	log.Println("Starting synthetic image publication monitor...")

	flag.Parse()
	app := &syntheticPublication{
		postEndpoint:      buildPostEndpoint(*postHost),
		s3Endpoint:        buildGetEndpoint(*s3Host, uuid),
		uuid:              uuid,
		latestImage:       make(chan postedData),
		latestPublication: make(chan publication),
		mutex:             &sync.Mutex{},
		history:           make([]publication, 0),
	}

	if *tick {
		tick := time.Tick(time.Minute)
		go func() {
			for {
				app.publish()
                                <- tick
			}
		}()
	}
	go app.checkPublishStatus()
	go app.historyManager()

	http.HandleFunc("/__health", fthealth.Handler("Synthetic publication monitor", "End-to-end image publication & monitor", app.healthcheck()))
	http.HandleFunc("/history", app.historyHandler)
	http.HandleFunc("/forcePublish", app.forcePublish)
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Println("Error: Could not start http server.")
	}
}

func (app *syntheticPublication) healthcheck() fthealth.Check {
    check := fthealth.Check{
        BusinessImpact: "Image publication doesn't work",
        Name: "End-to-end test",
        PanicGuide: "Contact #co-co channel on Slack",
        Severity: 3,
        TechnicalSummary: "Lots of things could have gone wrong. Check the /history endpoint for more info",
        Checker: app.lastPublicationStatus,
    }
    return check
}

func (app *syntheticPublication) lastPublicationStatus() error {
    n := len(app.history)
    if n != 0 && !app.history[n-1].succeeded {
        return errors.New("Publication failed.")
    }
    return nil
}

func (app *syntheticPublication) historyHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("History request.")
	app.mutex.Lock()
	for i := len(app.history) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "%d. { Date: %s, Published: %t, Error msg: %s}\n\n",
			len(app.history)-i,
			app.history[i].time.String(),
			app.history[i].succeeded,
			app.history[i].errorMsg,
		)
	}
	app.mutex.Unlock()
}

func (app *syntheticPublication) forcePublish(w http.ResponseWriter, r *http.Request) {
	log.Printf("Force publish.")
	err := app.publish()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Force publish failed. %s", err.Error())
	}
}

func (app *syntheticPublication) publish() error {
	eom, time := BuildRandomEOMImage(uuid)
	json, err := json.Marshal(eom)
	if err != nil {
		log.Printf("JSON marshalling failed. %s", err.Error())
		return err
	}
	buf := bytes.NewReader(json)

	client := http.Client{}
	req, err := http.NewRequest("POST", app.postEndpoint, buf)
	if err != nil {
		log.Printf("Error: Creating request failed. %s", err.Error())
		return err
	}
	req.Header.Add("X-Request-Id", "SYNTHETIC-REQ-MON_"+uniuri.NewLen(10))
	req.Header.Add("X-Origin-System-Id", "methode-web-pub")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error: Executing request failed. %s", err.Error())
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errMsg := fmt.Sprintf("Publishing failed at first step: could not post data to CMS notifier. Status code: %d", resp.StatusCode)
		app.latestPublication <- publication{time, false, errMsg}
	} else {
		app.latestImage <- postedData{time, eom.Value}
	}

	return nil
}

const generalErrMsg = "Internal error. "

func (app *syntheticPublication) checkPublishStatus() {
	for {
		latest := <-app.latestImage
		sentImg, err := base64.StdEncoding.DecodeString(latest.img)
		if err != nil {
			errMsg := fmt.Sprintf("Error: Decoding image received from channel failed. %s", err.Error())
			log.Printf(errMsg)
			app.latestPublication <- publication{latest.time, false, generalErrMsg + errMsg}
			continue
		}
		time.Sleep(30 * time.Second)
		resp, err := http.Get(app.s3Endpoint)
		if err != nil {
			errMsg := fmt.Sprintf("Error: Get request to s3 failed. %s", err.Error())
			log.Printf(errMsg)
			app.latestPublication <- publication{latest.time, false, generalErrMsg + errMsg}
			continue
		}
		defer resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound:
			errMsg := fmt.Sprint("Error: Image not found.")
			log.Println(errMsg)
			app.latestPublication <- publication{latest.time, false, generalErrMsg + errMsg}
			continue
		default:
			errMsg := fmt.Sprint("Error: Get request does not return 200 status.")
			log.Println(errMsg)
			app.latestPublication <- publication{latest.time, false, generalErrMsg + errMsg}
			continue
		}

		receivedImg, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			errMsg := fmt.Sprintf("Error: Could not read resp body. %s", err.Error())
			log.Printf(errMsg)
			app.latestPublication <- publication{latest.time, false, generalErrMsg + errMsg}
			continue
		}

		equals, msg := areEqual(sentImg, receivedImg)
		app.latestPublication <- publication{latest.time, equals, msg}
	}
}

func (app *syntheticPublication) historyManager() {
	for {
		latest := <-app.latestPublication

		app.mutex.Lock()
		if len(app.history) == 10 {
			app.history = app.history[1:len(app.history)]
		}
		app.history = append(app.history, latest)
		app.mutex.Unlock()
	}
}

func areEqual(b1, b2 []byte) (bool, string) {
	if bytes.Equal(b1, b2) {
		return true, ""
	} else {
		return false, "The sent and received images are not equal."
	}
}

func buildPostEndpoint(host string) string {
	return "http://" + host + "/notify"
}

func buildGetEndpoint(host, uuid string) string {
	return "http://" + host + "/" + uuid
}
