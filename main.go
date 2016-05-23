package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/cihub/seelog"
	goflags "github.com/jessevdk/go-flags"
	pb "gopkg.in/cheggaaa/pb.v1"
)

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	c := Config{
		FlushLock: sync.Mutex{},
	}

	// parse args
	_, err := goflags.Parse(&c)
	if err != nil {
		log.Error(err)
		return
	}

	setInitLogging(c.LogLevel)


	if(len(c.SourceEs)==0&&len(c.DumpInputFile)==0){
		log.Error("no input, type --help for more details")
		return
	}
	if(len(c.TargetEs)==0&&len(c.DumpOutFile)==0){
		log.Error("no output, type --help for more details")
		return
	}

	if(c.SourceEs==c.TargetEs&&c.SourceIndexNames==c.TargetIndexName){
		log.Error("migration output is the same as the output")
		return
	}

	// enough of a buffer to hold all the search results across all workers
	c.DocChan = make(chan map[string]interface{}, c.DocBufferCount*c.Workers*10)


	//dealing with basic auth
	if(len(c.SourceEsAuthStr)>0&&strings.Contains(c.SourceEsAuthStr,":")){
		authArray:=strings.Split(c.SourceEsAuthStr,":")
		auth:=Auth{User:authArray[0],Pass:authArray[1]}
		c.SourceAuth =&auth
	}

	if(len(c.TargetEsAuthStr)>0&&strings.Contains(c.TargetEsAuthStr,":")){
		authArray:=strings.Split(c.TargetEsAuthStr,":")
		auth:=Auth{User:authArray[0],Pass:authArray[1]}
		c.TargetAuth =&auth
	}

	//get source es version
	srcESVersion, errs := c.ClusterVersion(c.SourceEs,c.SourceAuth)
	if errs != nil {
		return
	}

	if strings.HasPrefix(srcESVersion.Version.Number,"5.") {
		log.Debug("source es is V5,",srcESVersion.Version.Number)
		api:=new(ESAPIV5)
		api.Host=c.SourceEs
		api.Auth=c.SourceAuth
		c.SourceESAPI = api
	} else {
		log.Debug("source es is not V5,",srcESVersion.Version.Number)
		api:=new(ESAPIV0)
		api.Host=c.SourceEs
		api.Auth=c.SourceAuth
		c.SourceESAPI = api
	}

	if(len(c.TargetEs)>0){
		//get target es version
		descESVersion, errs := c.ClusterVersion(c.TargetEs,c.TargetAuth)
		if errs != nil {
			return
		}

		if strings.HasPrefix(descESVersion.Version.Number,"5.") {
			log.Debug("target es is V5,",descESVersion.Version.Number)
			api:=new(ESAPIV5)
			api.Host=c.TargetEs
			api.Auth=c.TargetAuth
			c.TargetESAPI = api
		} else {
			log.Debug("target es is not V5,",descESVersion.Version.Number)
			api:=new(ESAPIV0)
			api.Host=c.TargetEs
			api.Auth=c.TargetAuth
			c.TargetESAPI = api

		}


	// wait for cluster state to be okay before moving
	timer := time.NewTimer(time.Second * 3)

	for {
		if status, ready := c.ClusterReady(c.SourceESAPI); !ready {
			log.Infof("%s at %s is %s, delaying migration ", status.Name, c.SourceEs, status.Status)
			<-timer.C
			continue
		}
		if status, ready := c.ClusterReady(c.TargetESAPI); !ready {
			log.Infof("%s at %s is %s, delaying migration ", status.Name, c.TargetEs, status.Status)
			<-timer.C
			continue
		}

		timer.Stop()
		break
	}


	// get all indexes from source
	indexNames,indexCount, sourceIndexMappings,err := c.SourceESAPI.GetIndexMappings(c.CopyAllIndexes,c.SourceIndexNames);
	if(err!=nil){
		log.Error(err)
		return
	}

	sourceIndexRefreshSettings:=map[string]interface{}{}

	if(indexCount>0){
		//override indexnames to be copy
		c.SourceIndexNames =indexNames

		// copy index settings if user asked
		if(c.CopyIndexSettings||c.ShardsCount>0){
			log.Info("start settings/mappings migration..")

			//get source index settings
			var sourceIndexSettings *Indexes
			sourceIndexSettings,err := c.SourceESAPI.GetIndexSettings(c.SourceIndexNames)
			log.Debug("source index settings:", sourceIndexSettings)
			if err != nil {
				log.Error(err)
				return
			}

			//get target index settings
			targetIndexSettings,err := c.TargetESAPI.GetIndexSettings(c.TargetIndexName);
			if(err!=nil){
				//ignore target es settings error
				log.Debug(err)
			}
			log.Debug("target IndexSettings", targetIndexSettings)


			//if there is only one index and we specify the dest indexname
			if((c.SourceIndexNames !=c.TargetIndexName)&&(indexCount==1||(len(c.TargetIndexName)>0))){
				log.Debug("only one index,so we can rewrite indexname")
				(*sourceIndexSettings)[c.TargetIndexName]=(*sourceIndexSettings)[c.SourceIndexNames]
				delete(*sourceIndexSettings,c.SourceIndexNames)
				log.Debug(sourceIndexSettings)
			}

			// dealing with indices settings
			for name, idx := range *sourceIndexSettings {
				log.Debug("dealing with index,name:",name,",settings:",idx)
				tempIndexSettings:=getEmptyIndexSettings()

				targetIndexExist:=false
				//if target index settings is exist and we don't copy settings, we use target settings
				if(targetIndexSettings!=nil){
					//if target es have this index and we dont copy index settings
					if val, ok := (*targetIndexSettings)[name]; ok {
						targetIndexExist=true
						tempIndexSettings=val.(map[string]interface{})
					}

					if(c.RecreateIndex){
						c.TargetESAPI.DeleteIndex(name)
						targetIndexExist=false
					}
				}

				//copy index settings
				if(c.CopyIndexSettings){
					tempIndexSettings=((*sourceIndexSettings)[name]).(map[string]interface{})
				}

				//check map elements
				if _, ok := tempIndexSettings["settings"]; !ok {
					tempIndexSettings["settings"] = map[string]interface{}{}
				}

				if _, ok := tempIndexSettings["settings"].(map[string]interface{})["index"]; !ok {
					tempIndexSettings["settings"].(map[string]interface{})["index"] = map[string]interface{}{}
				}

				sourceIndexRefreshSettings[name]=((*sourceIndexSettings)[name].(map[string]interface{}))["settings"].(map[string]interface{})["index"].(map[string]interface{})["refresh_interval"]

				//set refresh_interval
				tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{})["refresh_interval"] = -1
				tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{})["number_of_replicas"] = 0

				//clean up settings
				delete(tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{}),"number_of_shards")


				//copy indexsettings and mappings
				if(targetIndexExist){
					log.Debug("update index with settings,",name,tempIndexSettings)
					err:=c.TargetESAPI.UpdateIndexSettings(name,tempIndexSettings)
					if err != nil {
						log.Error(err)
					}
				}else{

					//override shard settings
					if(c.ShardsCount>0){
						tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{})["number_of_shards"] = c.ShardsCount
					}

					log.Debug("create index with settings,",name,tempIndexSettings)
					err:=c.TargetESAPI.CreateIndex(name,tempIndexSettings)
					if err != nil {
						log.Error(err)
					}


				}


			}

			if(c.CopyIndexMappings){
				log.Debug("start process with mappings")
				if(descESVersion.Version.Number[0]!=srcESVersion.Version.Number[0]){
					log.Error(srcESVersion.Version,"=>",descESVersion.Version,",cross-big-version mapping migration not avaiable, please update mapping manually :(")
					return
				}

				//if there is only one index and we specify the dest indexname
				if((c.SourceIndexNames !=c.TargetIndexName)&&(indexCount==1||(len(c.TargetIndexName)>0))){
					log.Debug("only one index,so we can rewrite indexname")
					(*sourceIndexMappings)[c.TargetIndexName]=(*sourceIndexMappings)[c.SourceIndexNames]
					delete(*sourceIndexMappings,c.SourceIndexNames)
					log.Debug(sourceIndexMappings)
				}

				for name, mapping := range *sourceIndexMappings {
					err:=c.TargetESAPI.UpdateIndexMapping(name,mapping.(map[string]interface{})["mappings"].(map[string]interface{}))
					if(err!=nil){
						log.Error(err)
					}
				}
			}

			log.Info("settings/mappings migration finished.")
		}


	}else{
		log.Error("index not exists,",c.SourceIndexNames)
		return
	}

		defer c.recoveryIndexSettings(sourceIndexRefreshSettings)
	}

	log.Info("start data migration..")

	// start scroll
	scroll, err := c.SourceESAPI.NewScroll(c.SourceIndexNames,c.ScrollTime,c.DocBufferCount,c.Query)
	if err != nil {
		log.Error(err)
		return
	}

	if scroll != nil && scroll.Hits.Docs != nil {
		if(scroll.Hits.Total==0){
			log.Error("can't find documents from source.")
			return
		}
		// create a progressbar and start a docCount
		fetchBar := pb.New(scroll.Hits.Total).Prefix("Pull ")
		outputBar := pb.New(scroll.Hits.Total).Prefix("Push ")


		// start pool
		pool, err := pb.StartPool(fetchBar, outputBar)
		if err != nil {
			panic(err)
		}

		wg := sync.WaitGroup{}
		//start es bulk thread
		if(len(c.TargetEs)>0){
			var docCount int
			wg.Add(c.Workers)
			for i := 0; i < c.Workers; i++ {
				go c.NewBulkWorker(&docCount, outputBar, &wg)
			}
		}else  if(len(c.DumpOutFile)>0){
			// start file write
			wg.Add(1)
			go c.NewFileDumpWorker(outputBar,&wg)
		}

		scroll.ProcessScrollResult(&c,fetchBar)

		// loop scrolling until done
		for scroll.Next(&c, fetchBar) == false {
		}
		fetchBar.Finish()

		// finished, close doc chan and wait for goroutines to be done
		close(c.DocChan)
		wg.Wait()
		outputBar.Finish()
		// close pool
		pool.Stop()
	}

	log.Info("data migration finished.")


}

func (c *Config)recoveryIndexSettings(sourceIndexRefreshSettings map[string]interface{})  {
	//update replica and refresh_interval
	for name,interval  := range  sourceIndexRefreshSettings{
		tempIndexSettings:=getEmptyIndexSettings()
		tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{})["refresh_interval"] = interval
		//tempIndexSettings["settings"].(map[string]interface{})["index"].(map[string]interface{})["number_of_replicas"] = 0
		c.TargetESAPI.UpdateIndexSettings(name,tempIndexSettings)
		if(c.Refresh){
			c.TargetESAPI.Refresh(name)
		}
	}
}

func (c *Config) ClusterVersion(host string,auth *Auth) (*ClusterVersion, []error) {

	url := fmt.Sprintf("%s", host)
	_, body, errs := Get(url,auth)
	if errs != nil {
		log.Error(errs)
		return nil,errs
	}

	log.Debug(body)

	version := &ClusterVersion{}
	err := json.Unmarshal([]byte(body), version)

	if err != nil {
		log.Error(body, errs)
		return nil,errs
	}
	return version,nil
}

func (c *Config) ClusterReady(api ESAPI) (*ClusterHealth, bool) {

	health := api.ClusterHealth()
	if health.Status == "red" {
		return health, false
	}

	if c.WaitForGreen == false && health.Status == "yellow" {
		return health, true
	}

	if health.Status == "green" {
		return health, true
	}

	return health, false
}
