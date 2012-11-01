package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	redisHost   = flag.String("redis-host", "", "Redis host in the form host:port:db.")
	defaultRate = flag.Float64("default-rate", 0.5, "Default rate to decay distributions with")
	nWorkers    = flag.Int("nworkers", 1, "Number of update workers that update the redis DB")
)

var rdb redis.Conn
var rLock sync.RWMutex
var updateChan chan *Distribution

type SingleResult struct {
	Distribution string  `json:"distribution"`
	Field        string  `json:"field"`
	Count        int     `json:"count"`
	Z            int     `json:"Z"`
	Probability  float64 `json:"probability"`
	Rate         float64 `json:"rate"`
}

type Distribution struct {
	Name string `json:"distribution"`
	Z    int    `json:"Z"`
	T    int
	Data map[string]int `json:"data"`
	Rate float64        `json:"rate"`
}

func (d *Distribution) Fill() error {
	rLock.RLock()
	data, err := redis.MultiBulk(rdb.Do("HGETALL", d.Name))
	rLock.RUnlock()

	if err != nil {
		return fmt.Errorf("Could not fetch data for %s: %s", d.Name, err)
	}

	// TODO: don't use the dist map to speed things up!
	d.Data = make(map[string]int)
	d.Z = 0
	for i := 0; i < len(data); i += 2 {
		k, err := redis.String(data[i], nil)
		if err != nil || k == "" {
			log.Printf("Could not read %s from distribution %s: %s", data[i], d.Name, err)
		}
		if k == "_R" {
			var rate float64
			n, err := fmt.Fscan(strings.NewReader(data[i+1].(string)), &rate)
			if n == 1 && err == nil {
				d.Rate = rate
			}
		} else {
			v, err := redis.Int(data[i+1], nil)
			if err != nil {
				log.Printf("Could not read %s from distribution %s: %s", data[i+1], d.Name, err)
			}
			if k == "_Z" {
				continue
			} else if k == "_T" {
				d.T = v
			} else {
				d.Data[k] = v
				d.Z += v
			}
		}
	}

	return nil
}

func (d *Distribution) Decay() {
	newZ := 0
	for k, v := range d.Data {
		d.Data[k], d.Z = Decay(v, d.Z, d.T, d.Rate)
		newZ += d.Data[k]
	}
	d.T = int(time.Now().Unix())
}

func UpdateRedis(readChan chan *Distribution) {
	var err error
	for dist := range readChan {
		log.Printf("Updating distribution: %s", dist.Name)
		if dist.Data == nil {
			dist.Fill()
			if err != nil {
				log.Printf("Could not update %s: %s", dist.Name, err)
				continue
			}
			dist.Decay()
		}

		rLock.Lock()
		rdb.Send("MULTI")
		for k, v := range dist.Data {
			rdb.Send("HSET", dist.Name, k, v)
		}
		rdb.Send("HSET", dist.Name, "_Z", dist.Z)
		rdb.Send("HSET", dist.Name, "_T", dist.T)
		_, err := rdb.Do("EXEC")
		rLock.Unlock()
		if err != nil {
			log.Printf("Could not update %s: %s", dist.Name, err)
		}
	}
}

func ConnectRedis() redis.Conn {
	parts := strings.Split(*redisHost, ":")

	if len(parts) != 3 {
		log.Panicf("redis-host must be in the form host:port:db")
	}

	db, err := redis.Dial("tcp", fmt.Sprintf("%s:%s", parts[0], parts[1]))
	if err == nil {
		ok, err := db.Do("SELECT", parts[2])
		if ok != "OK" || err != nil {
			log.Panicf("Could not change to DB %s: %s", parts[2], ok)
		}
	} else {
		log.Panicf("Could not connect: %s", err)
	}

	log.Printf("Connected to %s", *redisHost)
	return db
}

func IncrHandler(w http.ResponseWriter, r *http.Request) {
	reqParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fmt.Fprintf(w, "Error decoding request URI: %s\n", r.URL.RawQuery)
		return
	}
	distribution := reqParams.Get("distribution")
	if distribution == "" {
		fmt.Fprintf(w, "Missing required parameter 'distribution'\n")
		return
	}
	field := reqParams.Get("field")
	if field == "" {
		fmt.Fprintf(w, "Missing required parameter 'field'\n")
		return
	}
	N_raw := reqParams.Get("N")
	var N int
	if N_raw == "" {
		N = 1
	} else {
		N, err = strconv.Atoi(N_raw)
		if err != nil {
			fmt.Fprintf(w, "Could not parse field 'N': %s", err)
			return
		}
	}

	rLock.Lock()
	rdb.Send("MULTI")
	rdb.Send("HINCRBY", distribution, field, N)
	rdb.Send("HINCRBY", distribution, "_Z", N)
	rdb.Send("HSETNX", distribution, "_T", int(time.Now().Unix()))
	_, err = rdb.Do("EXEC")
	rLock.Unlock()

	if err == nil {
		fmt.Fprintf(w, "OK")
	} else {
		fmt.Fprintf(w, "FAIL")
	}
	updateChan <- &Distribution{Name: distribution}
}

func DistHandler(w http.ResponseWriter, r *http.Request) {
	reqParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fmt.Fprintf(w, "Error decoding request URI: %s\n", r.URL.RawQuery)
		return
	}
	distribution := reqParams.Get("distribution")
	if distribution == "" {
		fmt.Fprintf(w, "Missing required parameter 'distribution'\n")
		return
	}
	var rate float64
	rate_raw := reqParams.Get("rate")
	if rate_raw == "" {
		rate = *defaultRate
	} else {
		n, err := fmt.Fscan(strings.NewReader(rate_raw), &rate)
		if n == 0 || err != nil {
			fmt.Fprintf(w, "Could not parse 'rate' field: %s", err)
		}
	}

	dist := Distribution{Name: distribution}
	err = dist.Fill()
	if err != nil {
		fmt.Fprintf(w, "Error retrieving distribution %s: %s", distribution, err)
		return
	}

	if dist.Rate == *defaultRate {
		dist.Rate = rate
	}

	dist.Decay()
	j, err := json.Marshal(dist)
	if err != nil {
		fmt.Fprintf(w, "Error formatting result: %s", err)
	} else {
		fmt.Fprintf(w, "%s", j)
	}

	updateChan <- &dist
}

func GetHandler(w http.ResponseWriter, r *http.Request) {
	reqParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fmt.Fprintf(w, "Error decoding request URI: %s\n", r.URL.RawQuery)
		return
	}
	distribution := reqParams.Get("distribution")
	if distribution == "" {
		fmt.Fprintf(w, "Missing required parameter 'distribution'\n")
		return
	}
	field := reqParams.Get("field")
	if field == "" {
		fmt.Fprintf(w, "Missing required parameter 'field'\n")
		return
	}
	var rate float64
	rate_raw := reqParams.Get("rate")
	if rate_raw == "" {
		rate = *defaultRate
	} else {
		n, err := fmt.Fscan(strings.NewReader(rate_raw), &rate)
		if n == 0 || err != nil {
			fmt.Fprintf(w, "Could not parse 'rate' field: %s", err)
		}
	}

	rLock.RLock()
	data, err := redis.MultiBulk(rdb.Do("HMGET", distribution, field, "_Z", "_T"))
	rLock.RUnlock()

	if err != nil || len(data) != 3 {
		fmt.Fprintf(w, "Error retrieving field %s: %s", field, err)
	} else {
		count, _ := redis.Int(data[0], nil)
		Z, _ := redis.Int(data[1], nil)
		t, _ := redis.Int(data[2], nil)

		count, Z = Decay(count, Z, t, rate)
		var p float64
		if Z == 0 {
			p = 0.0
		} else {
			p = float64(count) / float64(Z)
		}

		result := SingleResult{
			Distribution: distribution,
			Field:        field,
			Count:        count,
			Z:            Z,
			Probability:  p,
			Rate:         rate,
		}
		j, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(w, "Error formatting result: %s", err)
		} else {
			fmt.Fprintf(w, "%s", j)
		}

		updateChan <- &Distribution{Name: distribution}
	}
}

func main() {
	flag.Parse()

	rdb = ConnectRedis()

	log.Printf("Starting %d update workers", *nWorkers)
	updateChan = make(chan *Distribution, 10) //25 * *nWorkers)
	for i := 0; i < *nWorkers; i++ {
		go UpdateRedis(updateChan)
	}

	http.HandleFunc("/get", GetHandler)
	http.HandleFunc("/incr", IncrHandler)
	http.HandleFunc("/dist", DistHandler)
	log.Fatal(http.ListenAndServe(":6666", nil))
}
