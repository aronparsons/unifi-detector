package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/mdlayher/unifi"
	"github.com/muesli/cache2go"
	flag "github.com/namsral/flag"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

const programName = "unifi-detector"

var programVersion string

type appConfig struct {
	scanInterval   time.Duration
	clientLifespan time.Duration
}

type unifiConfig struct {
	address  string
	username string
	password string
	insecure bool
	timeout  time.Duration
}

type mqttConfig struct {
	address  string
	username string
	password string
	topic    string
	qos      byte
	retain   bool
	client   *mqtt.Client
}

type ntfyConfig struct {
	topic string
}

type mqttHeartbeatMsg struct {
	Timestamp time.Time `json:"timestamp"`
	Heartbeat bool      `json:"heartbeat"`
	Clients   int       `json:"clients"`
}

type mqttClientMsg struct {
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Hostname  string    `json:"hostname"`
	MAC       string    `json:"mac"`
	IP        net.IP    `json:"ip"`
}

func newCache(name string) *cache2go.CacheTable {
	return cache2go.Cache(name)
}

func newClient(config *unifiConfig) (*unifi.Client, error) {
	httpClient := &http.Client{Timeout: config.timeout}
	if config.insecure {
		httpClient = unifi.InsecureHTTPClient(config.timeout)
	}

	client, err := unifi.NewClient(config.address, httpClient)
	if err != nil {
		return nil, fmt.Errorf("cannot create UniFi controller client: %v", err)
	}
	client.UserAgent = "git.home.lan/scraton/unifi-detector"

	if err := client.Login(config.username, config.password); err != nil {
		return nil, fmt.Errorf("failed to authenticate to UniFi controller: %v", err)
	}

	return client, err
}

func pollClients(config *appConfig, unifiClient *unifi.Client, mqtt *mqttConfig, ntfy *ntfyConfig, cache *cache2go.CacheTable) {
	for range time.Tick(config.scanInterval) {
		go evaluateClients(config, unifiClient, cache, mqtt, ntfy, false)
	}
}

func initializeClientsCache(config *appConfig, unifiClient *unifi.Client, mqtt *mqttConfig, ntfy *ntfyConfig, cache *cache2go.CacheTable) {
	for {
		err := evaluateClients(config, unifiClient, cache, mqtt, ntfy, true)

		if err != nil {
			log.Errorf("failed to initialize client cache")
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}
}

func notifyOfClient(client *unifi.Station, mqtt *mqttConfig) {
	clientMsg := &mqttClientMsg{
		FirstSeen: client.FirstSeen,
		LastSeen:  client.LastSeen,
		Hostname:  client.Hostname,
		MAC:       client.MAC.String(),
		IP:        client.IP,
	}
	msg, err := json.Marshal(clientMsg)
	if err != nil {
		log.Errorf("failed to generate notification: %v\n", err)
		return
	}

	(*mqtt.client).Publish(mqtt.topic, mqtt.qos, mqtt.retain, string(msg))
	log.Debugf("notified mqtt of client %v", client.MAC.String())
}

func notifyOfClientNtfy(client *unifi.Station, ntfy *ntfyConfig) {
	clientMsg := &mqttClientMsg{
		FirstSeen: client.FirstSeen,
		LastSeen:  client.LastSeen,
		Hostname:  client.Hostname,
		MAC:       client.MAC.String(),
		IP:        client.IP,
	}

	msg := fmt.Sprintf("new client: %+v", clientMsg)

	log.Infof("notifying ntfy topic %s of client %v", ntfy.topic, client.MAC.String())

	httpClient := &http.Client{}
	_, err := httpClient.Post(fmt.Sprintf("https://ntfy.sh/%s", ntfy.topic), "text/plain", strings.NewReader(msg))

	if err != nil {
		log.Errorf("error making http request: %s\n", err)
	}
}

func evaluateClients(config *appConfig, unifiClient *unifi.Client, cache *cache2go.CacheTable, mqtt *mqttConfig, ntfy *ntfyConfig, firstRun bool) error {
	clients, err := unifiClient.Stations("default")
	if err != nil {
		log.Errorf("failed to fetch clients: %v\n", err)
		return err
	}

	heartbeatMsg := &mqttHeartbeatMsg{
		Timestamp: time.Now(),
		Heartbeat: true,
		Clients:   len(clients),
	}
	msg, err := json.Marshal(heartbeatMsg)
	if err != nil {
		log.Errorf("failed to generate heartbeat: %v\n", err)
		return err
	}

	if mqtt.client != nil {
		(*mqtt.client).Publish(mqtt.topic, mqtt.qos, mqtt.retain, string(msg))
		log.Info("issued heartbeat")
	}

	// Evaluate clients
	for _, c := range clients {
		timeSinceLastSeen := time.Now().UTC().Sub(c.LastSeen)
		cachedClient, err := cache.Value(c.MAC.String())

		if err != nil && !firstRun {
			log.WithFields(log.Fields{
				"mac":      c.MAC.String(),
				"hostname": c.Hostname,
				"ip":       c.IP.String(),
			}).Info("new client discovered")

			if timeSinceLastSeen <= config.clientLifespan {
				if mqtt.client != nil {
					go notifyOfClient(c, mqtt)
				}

				if ntfy.topic != "" {
					go notifyOfClientNtfy(c, ntfy)
				}
			}
		}

		if timeSinceLastSeen <= config.clientLifespan {
			cache.Add(c.MAC.String(), config.clientLifespan, c)
		} else {
			log.WithFields(log.Fields{
				"mac":               c.MAC.String(),
				"hostname":          c.Hostname,
				"ip":                c.IP.String(),
				"timeSinceLastSeen": timeSinceLastSeen.String(),
			}).Info("ignoring client; older than the lifespan")

			if cachedClient != nil {
				// ensure client is evicted
				cache.Delete(c.MAC.String())
			}
		}
	}

	return nil
}

func main() {
	var (
		config         appConfig
		configLifespan int
		configInterval int
		clientTimeout  int
		clientConfig   unifiConfig
		mqttConfig     mqttConfig
		mqttQos        int
		ntfyConfig     ntfyConfig
		printVersion   bool
		debug          bool
	)

	fs := flag.NewFlagSetWithEnvPrefix(os.Args[0], "UNIFI", 0)
	fs.IntVar(&configLifespan, "lifespan", 86400, "Client cache lifespan in seconds")
	fs.IntVar(&configInterval, "interval", 60, "Scan interval in seconds")

	fs.StringVar(&clientConfig.address, "api-address", "", "Unifi Controller address")
	fs.StringVar(&clientConfig.username, "api-user", "", "Unifi Controller username")
	fs.StringVar(&clientConfig.password, "api-password", "", "Unifi Controller password")
	fs.BoolVar(&clientConfig.insecure, "api-insecure", false, "Allow insecure connection to Unifi Controller")
	fs.IntVar(&clientTimeout, "api-timeout", 60, "Timeout for connecting to Unfi Controller")

	fs.StringVar(&mqttConfig.address, "mqtt-address", "", "MQTT broker address")
	fs.StringVar(&mqttConfig.username, "mqtt-user", "", "MQTT broker username")
	fs.StringVar(&mqttConfig.password, "mqtt-password", "", "MQTT broker password")
	fs.StringVar(&mqttConfig.topic, "mqtt-topic", "", "MQTT broker topic")
	fs.IntVar(&mqttQos, "mqtt-qos", 0, "MQTT QoS for messages sent")
	fs.BoolVar(&mqttConfig.retain, "mqtt-retain", true, "Retain MQTT messages on broker")

	fs.StringVar(&ntfyConfig.topic, "ntfy-topic", "", "ntfy topic")

	fs.BoolVar(&printVersion, "version", false, "displays version information")
	fs.BoolVar(&debug, "debug", false, "print debug logs")

	e := fs.Parse(os.Args[1:])

	switch e {
	case nil:
		// do nothing
	case flag.ErrHelp:
		os.Exit(0)
	default:
		log.Fatal(e)
		os.Exit(1)
	}

	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	versionString := fmt.Sprintf("%v version:%s", programName, programVersion)
	if printVersion {
		fmt.Println(versionString)
		os.Exit(0)
	}

	if clientConfig.address == "" {
		log.Fatal("hostname for Unifi Controller must be set")
	}

	if mqttConfig.address == "" && ntfyConfig.topic == "" {
		log.Warningf("mqtt or ntfy should be enabled")
	}

	if mqttConfig.address != "" && mqttConfig.topic == "" {
		log.Fatal("topic for MQTT broker must be set")
	}

	config.scanInterval = time.Duration(configInterval) * time.Second
	config.clientLifespan = time.Duration(configLifespan) * time.Second
	clientConfig.timeout = time.Duration(clientTimeout) * time.Second
	mqttConfig.qos = byte(mqttQos)

	log.Info(versionString)

	// initialize cache
	cache := newCache("unifi_clients")

	// initialize unifi client
	client, err := newClient(&clientConfig)
	if err != nil {
		log.Fatalf("failed to connect to UniFi: %v", err)
		os.Exit(1)
	} else {
		log.Infof("successfully connected to UniFi at %v", clientConfig.address)
	}

	// initialize mqtt connection
	if mqttConfig.address != "" {
		mqttOpts := mqtt.NewClientOptions().AddBroker(mqttConfig.address).SetClientID(programName)
		mqttOpts.SetAutoReconnect(true)
		mqttOpts.SetKeepAlive(2 * time.Second)
		mqttOpts.SetPingTimeout(1 * time.Second)
		mqttOpts.SetUsername(mqttConfig.username)
		mqttOpts.SetPassword(mqttConfig.password)

		mqttClient := mqtt.NewClient(mqttOpts)
		if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
			log.Fatalf("failed to connect to MQTT: %v", token.Error())
			os.Exit(1)
		} else {
			log.Infof("successfully to MQTT broker at %v", mqttConfig.address)
		}
		mqttConfig.client = &mqttClient
	}

	// fetch initial list of clients
	initializeClientsCache(&config, client, &mqttConfig, &ntfyConfig, cache)

	// start polling for clients
	pollClients(&config, client, &mqttConfig, &ntfyConfig, cache)
}
