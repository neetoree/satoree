package main

import (
	"github.com/fsouza/go-dockerclient"
	"github.com/coreos/go-etcd/etcd"
	"log"
	"flag"
	"os"
	"fmt"
	"strconv"
	"strings"
	"net"
	"time"
)

var etcdUrl = flag.String("etcd", "http://127.0.0.1:2379", "etcd endpoint")
var skydnsDomain = flag.String("domain", "skydns.local", "skydns domain")
var skydnsLocal = flag.String("local", unpanic(os.Hostname()).(string) + ".nodes.skydns.local", "skydns local part")
var etcdTtl = flag.Int("ttl", 30, "TTL in seconds")
var etcdCheck = flag.Int("check", 15, "heartbeat interval in seconds")
var externalIp = flag.String("ip", "", "external ip for global records")
var localPrefix, globalPrefix string

var waiters map[string]chan bool = make(map[string]chan bool)

func unpanic(first interface{}, err error) interface{} {
	if err != nil {
		panic(err)
	}
	return first
}

func main()  {
	flag.Usage = func() {
		fmt.Printf("Usage of %s:\n", os.Args[0])
		fmt.Printf("  %s [options]\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if *externalIp == "" {
		fmt.Println("Missing external ip")
		flag.Usage()
		return
	}

	globalPrefix = preparePrefix(*skydnsDomain)
	localPrefix = preparePrefix(*skydnsLocal)

	log.Printf("Started satoree on domain %s with local %s", *skydnsDomain, *skydnsLocal)
	etcdApi := etcd.NewClient([]string{*etcdUrl})

	dockerApi, err := docker.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	listener := make(chan *docker.APIEvents)
	err = dockerApi.AddEventListener(listener)
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		err = dockerApi.RemoveEventListener(listener)
		if err != nil {
			log.Fatal(err)
		}
	}()

	list, err := dockerApi.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		panic(nil)
	}

	for _, apicont := range list {
		container, err := dockerApi.InspectContainer(apicont.ID)
		if err != nil {
			panic(err)
		}
		registerServices(etcdApi, container)
	}

	for {
		msg := <-listener
		switch msg.Type {
		case "container":
			switch msg.Action {
			case "die": fallthrough
			case "stop":
				container, err := dockerApi.InspectContainer(msg.Actor.ID)
				if err != nil {
					log.Fatal(err)
				}
				unrgisterServices(etcdApi, container)
			case "start":
				container, err := dockerApi.InspectContainer(msg.Actor.ID)
				if err != nil {
					log.Fatal(err)
				}
				registerServices(etcdApi, container)
			}
		}
	}
}
func preparePrefix(unprepared string) string {
	split := strings.Split(unprepared, ".")
	for i, j := 0, len(split)-1; i < j; i, j = i+1, j-1 {
		split[i], split[j] = split[j], split[i]
	}
	return "/skydns/" + strings.Join(split, "/")
}

func registerServices(etcd *etcd.Client, container *docker.Container) {
	log.Println("start", container.ID)
	idx := 0;

	waiters[container.ID] = make(chan bool)

	for _, network := range container.NetworkSettings.Networks {
		for port, portbinds := range container.NetworkSettings.Ports {
			idx++;
			localPath := makePath(localPrefix, container.Name, port, idx)
			globalPath := makePath(globalPrefix, container.Name, port, idx)

			if len(portbinds) == 0 {
				_, err := etcd.Set(localPath, makeRecord(network.IPAddress, port.Port(), 15), uint64(*etcdTtl))
				if err != nil {
					panic(err)
				}
				go heartbeat(etcd, network.IPAddress, port, localPath, container.Name, waiters[container.ID])
			} else {
				_, err := etcd.Set(globalPath, makeRecord(*externalIp, port.Port(), 30), uint64(*etcdTtl))
				if err != nil {
					panic(err)
				}
				go heartbeat(etcd, *externalIp, port, globalPath, container.Name, waiters[container.ID])
			}
		}
	}
}
func heartbeat(etcd *etcd.Client, host string, port docker.Port, path string, name string, ch chan bool) {
	for {
		conn, err := net.Dial(port.Proto(), host + ":" + string(port.Port()))
		if err != nil {
			etcd.Delete(path, true)
		} else {
			conn.Close()
			resp, err := etcd.Get(path, false, false);
			if (err != nil) {
				panic(err)
			}
			etcd.Update(path, resp.Node.Value, uint64(*etcdTtl))
		}
		time.Sleep(time.Second * time.Duration(*etcdCheck))
		select {
		case _, more := <-ch:
			if !more {
				removeEndpoints(etcd, name)
				return
			}
		default:
			continue
		}
	}
}


func makePath(prefix string, name string, port docker.Port, idx int) string {
	return prefix + "/" + name + "/" + port.Port() + "/" + port.Proto() + "/" + strconv.Itoa(idx);
}

func makeRecord(ip string, port string, priority int) string {
	return `{"host":"` + ip + `","port":` + port + `,"priority":` + strconv.Itoa(priority) + `}`
}

func unrgisterServices(etcd *etcd.Client, container *docker.Container) {
	log.Println("stop", container.ID)
	removeEndpoints(etcd, container.Name)
	close(waiters[container.ID])
	delete(waiters, container.ID)
}

func removeEndpoints(etcd *etcd.Client, name string) {
	etcd.Delete(localPrefix + "/" + name, true)
	etcd.Delete(globalPrefix + "/" + name, true)
}