package config_client

import (
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/kms"
	"github.com/nacos-group/nacos-sdk-go/clients/cache"
	"github.com/nacos-group/nacos-sdk-go/clients/nacos_client"
	"github.com/nacos-group/nacos-sdk-go/common/constant"
	"github.com/nacos-group/nacos-sdk-go/common/http_agent"
	"github.com/nacos-group/nacos-sdk-go/common/logger"
	"github.com/nacos-group/nacos-sdk-go/common/nacos_error"
	"github.com/nacos-group/nacos-sdk-go/common/util"
	"github.com/nacos-group/nacos-sdk-go/model"
	"github.com/nacos-group/nacos-sdk-go/utils"
	"github.com/nacos-group/nacos-sdk-go/vo"
)

type ConfigClient struct {
	nacos_client.INacosClient
	kmsClient      *kms.Client
	localConfigs   []vo.ConfigParam
	mutex          sync.Mutex
	configProxy    ConfigProxy
	configCacheDir string
}

const perTaskConfigSize = 3000

var (
	currentTaskCount int
	cacheMap         cache.ConcurrentMap
)

type cacheData struct {
	isInitializing    bool
	dataId            string
	group             string
	content           string
	tenant            string
	cacheDataListener *cacheDataListener
	md5               string
	appName           string
	taskId            int
	configClient      *ConfigClient
}

type cacheDataListener struct {
	listener vo.Listener
	lastMd5  string
}

func NewConfigClient(nc nacos_client.INacosClient) (ConfigClient, error) {
	config := ConfigClient{}
	config.INacosClient = nc
	clientConfig, err := nc.GetClientConfig()
	if err != nil {
		return config, err
	}
	serverConfig, err := nc.GetServerConfig()
	if err != nil {
		return config, err
	}
	httpAgent, err := nc.GetHttpAgent()
	if err != nil {
		return config, err
	}
	err = logger.InitLog(clientConfig.LogDir)
	if err != nil {
		return config, err
	}
	config.configCacheDir = clientConfig.CacheDir + string(os.PathSeparator) + "config"
	config.configProxy, err = NewConfigProxy(serverConfig, clientConfig, httpAgent)
	if clientConfig.OpenKMS {
		kmsClient, err := kms.NewClientWithAccessKey(clientConfig.RegionId, clientConfig.AccessKey, clientConfig.SecretKey)
		if err != nil {
			return config, err
		}
		config.kmsClient = kmsClient
	}
	cacheMap = cache.NewConcurrentMap()
	go delayScheduler(1*time.Millisecond, 10*time.Millisecond, listenConfigExecutor())
	return config, err
}

func (client *ConfigClient) sync() (clientConfig constant.ClientConfig,
	serverConfigs []constant.ServerConfig, agent http_agent.IHttpAgent, err error) {
	clientConfig, err = client.GetClientConfig()
	if err != nil {
		log.Println(err, ";do you call client.SetClientConfig()?")
	}
	if err == nil {
		serverConfigs, err = client.GetServerConfig()
		if err != nil {
			log.Println(err, ";do you call client.SetServerConfig()?")
		}
	}
	if err == nil {
		agent, err = client.GetHttpAgent()
		if err != nil {
			log.Println(err, ";do you call client.SetHttpAgent()?")
		}
	}
	return
}

func (client *ConfigClient) GetConfig(param vo.ConfigParam) (content string, err error) {
	content, err = client.getConfigInner(param)

	if err != nil {
		return "", err
	}

	return client.decrypt(param.DataId, content)
}

func (client *ConfigClient) decrypt(dataId, content string) (string, error) {
	if strings.HasPrefix(dataId, "cipher-") && client.kmsClient != nil {
		request := kms.CreateDecryptRequest()
		request.Method = "POST"
		request.Scheme = "https"
		request.AcceptFormat = "json"
		request.CiphertextBlob = content
		response, err := client.kmsClient.Decrypt(request)
		if err != nil {
			return "", errors.New("kms decrypt failed")
		}
		content = response.Plaintext
	}

	return content, nil
}

func (client *ConfigClient) getConfigInner(param vo.ConfigParam) (content string, err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.GetConfig] param.dataId can not be empty")
		return "", err
	}
	if len(param.Group) <= 0 {
		err = errors.New("[client.GetConfig] param.group can not be empty")
		return "", err
	}
	clientConfig, _ := client.GetClientConfig()
	cacheKey := utils.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId)
	content, err = client.configProxy.GetConfigProxy(param, clientConfig.NamespaceId, clientConfig.AccessKey, clientConfig.SecretKey)

	if err != nil {
		log.Printf("[ERROR] get config from server error:%s ", err.Error())
		if _, ok := err.(*nacos_error.NacosError); ok {
			nacosErr := err.(*nacos_error.NacosError)
			if nacosErr.ErrorCode() == "404" {
				cache.WriteConfigToFile(cacheKey, client.configCacheDir, "")
				return "", errors.New("config not found")
			}
			if nacosErr.ErrorCode() == "403" {
				return "", errors.New("get config forbidden")
			}
		}
		content, err = cache.ReadConfigFromFile(cacheKey, client.configCacheDir)
		if err != nil {
			log.Printf("[ERROR] get config from cache  error:%s ", err.Error())
			return "", errors.New("read config from both server and cache fail")
		}

	} else {
		cache.WriteConfigToFile(cacheKey, client.configCacheDir, content)
	}
	return content, nil
}

func (client *ConfigClient) PublishConfig(param vo.ConfigParam) (published bool,
	err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.PublishConfig] param.dataId can not be empty")
	}
	if len(param.Group) <= 0 {
		err = errors.New("[client.PublishConfig] param.group can not be empty")
	}
	if len(param.Content) <= 0 {
		err = errors.New("[client.PublishConfig] param.content can not be empty")
	}
	clientConfig, _ := client.GetClientConfig()
	return client.configProxy.PublishConfigProxy(param, clientConfig.NamespaceId, clientConfig.AccessKey, clientConfig.SecretKey)
}

func (client *ConfigClient) DeleteConfig(param vo.ConfigParam) (deleted bool,
	err error) {
	if len(param.DataId) <= 0 {
		err = errors.New("[client.DeleteConfig] param.dataId can not be empty")
	}
	if len(param.Group) <= 0 {
		err = errors.New("[client.DeleteConfig] param.group can not be empty")
	}

	clientConfig, _ := client.GetClientConfig()
	return client.configProxy.DeleteConfigProxy(param, clientConfig.NamespaceId, clientConfig.AccessKey, clientConfig.SecretKey)
}

//Cancel Listen Config
func (client *ConfigClient) CancelListenConfig(param *vo.ConfigParam) (err error) {
	clientConfig, err := client.GetClientConfig()
	if err != nil {
		log.Fatalf("[checkConfigInfo.GetClientConfig] failed.")
		return
	}
	cacheMap.Remove(utils.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId))
	log.Printf("Cancel listen config DataId:%s Group:%s", param.DataId, param.Group)
	return err
}

func (client *ConfigClient) ListenConfig(param vo.ConfigParam) (err error) {
	if len(param.DataId) <= 0 {
		log.Fatalf("[client.ListenConfig] DataId can not be empty")
		return
	}
	if len(param.Group) <= 0 {
		log.Fatalf("[client.ListenConfig] Group can not be empty")
		return
	}
	clientConfig, err := client.GetClientConfig()
	if err != nil {
		log.Fatalf("[checkConfigInfo.GetClientConfig] failed.")
		return
	}
	//todo 1：监听onChange fun只支持一个
	key := utils.GetConfigCacheKey(param.DataId, param.Group, clientConfig.NamespaceId)
	var cData cacheData
	if v, ok := cacheMap.Get(key); ok {
		cData = v.(cacheData)
		cData.isInitializing = true
	} else {
		content, err := cache.ReadConfigFromFile(key, client.configCacheDir)
		if err != nil {
			log.Printf("[cache.ReadConfigFromFile] error:[%s]", err.Error())
			content = ""
		}
		md5Str := util.Md5(content)
		listener := cacheDataListener{
			listener: param.OnChange,
			lastMd5:  md5Str,
		}
		cData = cacheData{
			isInitializing:    true,
			appName:           param.AppName,
			dataId:            param.DataId,
			group:             param.Group,
			tenant:            clientConfig.NamespaceId,
			content:           content,
			md5:               md5Str,
			cacheDataListener: &listener,
			taskId:            len(cacheMap.Keys()) / perTaskConfigSize,
			configClient:      client,
		}
	}
	cacheMap.Set(key, cData)
	return
}

//Delay Scheduler
//initialDelay the time to delay first execution
//delay the delay between the termination of one execution and the commencement of the next
func delayScheduler(initialDelay, delay time.Duration, execute func()) {
	t := time.NewTimer(initialDelay)
	defer t.Stop()

	for {
		<-t.C
		execute()
		t.Reset(delay)
	}
}

//Listen for the configuration executor
func listenConfigExecutor() func() {
	return func() {
		listenerSize := len(cacheMap.Keys())
		taskCount := int(math.Ceil(float64(listenerSize) / float64(perTaskConfigSize)))
		if taskCount > currentTaskCount {
			for i := currentTaskCount; i < taskCount; i++ {
				go delayScheduler(1*time.Millisecond, 10*time.Millisecond, longPulling(i))
			}
			currentTaskCount = taskCount
		}
	}
}

//Long polling listening configuration
func longPulling(taskId int) func() {
	return func() {
		var listeningConfigs string
		var client *ConfigClient
		isInitializing := false
		for _, key := range cacheMap.Keys() {
			if value, ok := cacheMap.Get(key); ok {
				cData := value.(cacheData)
				client = cData.configClient
				if cData.isInitializing {
					isInitializing = true
				}
				if cData.taskId == taskId {
					if len(cData.tenant) > 0 {
						listeningConfigs += cData.dataId + constant.SPLIT_CONFIG_INNER + cData.group + constant.SPLIT_CONFIG_INNER +
							cData.md5 + constant.SPLIT_CONFIG_INNER + cData.tenant + constant.SPLIT_CONFIG
					} else {
						listeningConfigs += cData.dataId + constant.SPLIT_CONFIG_INNER + cData.group + constant.SPLIT_CONFIG_INNER +
							cData.md5 + constant.SPLIT_CONFIG
					}
				}
			}
		}

		if len(listeningConfigs) > 0 {
			clientConfig, err := client.GetClientConfig()
			if err != nil {
				log.Println("[checkConfigInfo.GetClientConfig] failed.")
				return
			}
			// http get
			params := make(map[string]string)
			params[constant.KEY_LISTEN_CONFIGS] = listeningConfigs

			var changed string
			changedTmp, err := client.configProxy.ListenConfig(params, isInitializing, clientConfig.AccessKey, clientConfig.SecretKey)
			if err == nil {
				changed = changedTmp
			} else {
				if _, ok := err.(*nacos_error.NacosError); ok {
					changed = changedTmp
				} else {
					log.Println("[client.ListenConfig] listen config error:", err.Error())
				}
			}
			if strings.ToLower(strings.Trim(changed, " ")) == "" {
				log.Println("[client.ListenConfig] no change")
			} else {
				log.Print("[client.ListenConfig] config changed:" + changed)
				client.callListener(changed, clientConfig.NamespaceId)
			}
		}
	}

}

//Execute the Listener callback func()
func (client *ConfigClient) callListener(changed, tenant string) {
	changedConfigs := strings.Split(changed, "%01")
	for _, config := range changedConfigs {
		attrs := strings.Split(config, "%02")
		if len(attrs) >= 2 {
			if value, ok := cacheMap.Get(utils.GetConfigCacheKey(attrs[0], attrs[1], tenant)); ok {
				cData := value.(cacheData)
				if content, err := client.getConfigInner(vo.ConfigParam{
					DataId: cData.dataId,
					Group:  cData.group,
				}); err != nil {
					log.Printf("[client.getConfigInner] DataId:[%s] Group:[%s] Error:[%s]", cData.dataId, cData.group, err.Error())
				} else {
					cData.content = content
					cData.md5 = util.Md5(content)
					if cData.md5 != cData.cacheDataListener.lastMd5 {
						cData.cacheDataListener.listener("", attrs[1], attrs[0], cData.content)
						cData.cacheDataListener.lastMd5 = cData.md5
						cData.isInitializing = false
						cacheMap.Set(utils.GetConfigCacheKey(cData.dataId, cData.group, tenant), cData)
					}
				}

			}
		}
	}
}

func (client *ConfigClient) buildBasePath(serverConfig constant.ServerConfig) (basePath string) {
	basePath = "http://" + serverConfig.IpAddr + ":" +
		strconv.FormatUint(serverConfig.Port, 10) + serverConfig.ContextPath + constant.CONFIG_PATH
	return
}

func (client *ConfigClient) SearchConfig(param vo.SearchConfigParm) (*model.ConfigPage, error) {
	return client.searchConfigInnter(param)
}

func (client *ConfigClient) searchConfigInnter(param vo.SearchConfigParm) (*model.ConfigPage, error) {
	if param.Search != "accurate" && param.Search != "blur" {
		return nil, errors.New("[client.searchConfigInnter] param.search must be accurate or blur")
	}
	if param.PageNo <= 0 {
		param.PageNo = 1
	}
	if param.PageSize <= 0 {
		param.PageSize = 10
	}
	clientConfig, _ := client.GetClientConfig()
	configItems, err := client.configProxy.SearchConfigProxy(param, clientConfig.NamespaceId, clientConfig.AccessKey, clientConfig.SecretKey)
	if err != nil {
		log.Printf("[ERROR] search config from server error:%s ", err.Error())
		if _, ok := err.(*nacos_error.NacosError); ok {
			nacosErr := err.(*nacos_error.NacosError)
			if nacosErr.ErrorCode() == "404" {
				return nil, errors.New("config not found")
			}
			if nacosErr.ErrorCode() == "403" {
				return nil, errors.New("get config forbidden")
			}
		}
		return nil, err
	}
	return configItems, nil
}
