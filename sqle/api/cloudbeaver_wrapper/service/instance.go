package service

import (
	"context"
	"fmt"

	gqlClient "github.com/actiontech/sqle/sqle/api/cloudbeaver_wrapper/graph/client"
	driverV2 "github.com/actiontech/sqle/sqle/driver/v2"
	"github.com/actiontech/sqle/sqle/log"
	sqleModel "github.com/actiontech/sqle/sqle/model"
)

func SyncUserBindInstance(cbUserID string) error {
	// 获取当前SQLE用户
	s := sqleModel.GetStorage()
	sqleUserCache, exist, err := s.GetCloudBeaverUserCacheByCBUserID(cbUserID)
	if err != nil || !exist { // 如果用户不存在表示这可能是个与SQLE无关的用户
		return err
	}
	sqleUser, exist, err := s.GetUserByID(sqleUserCache.SQLEUserID)
	if err != nil || !exist {
		return err
	}

	// 获取用户当前拥有权限的SQLE实例
	var sqleInstSlice []*sqleModel.Instance
	if sqleUser.Name == sqleModel.DefaultAdminUser {
		sqleInstSlice, err = s.GetAllInstance()
	} else {
		sqleInstSlice, err = s.GetUserCanOpInstances(sqleUser, []uint{sqleModel.OP_SQL_QUERY_QUERY})
	}
	if err != nil {
		return err
	}

	sqleInstMap := map[uint] /*sqle inst id*/ *sqleModel.Instance{}
	instIds := make([]uint, len(sqleInstSlice))
	for i, instance := range sqleInstSlice {
		instIds[i] = instance.ID
	}
	insts, err := s.GetInstancesFromActiveProjectByIds(instIds)
	if err != nil {
		return err
	}
	for _, inst := range insts {
		sqleInstMap[inst.ID] = inst
	}

	// 同步实例信息
	if err := SyncInstance(sqleInstMap); err != nil {
		return err
	}

	// 同步cloudbeaver上用户对实例的权限信息
	if err := SyncVisibleInstance(sqleUserCache, sqleUser, sqleInstMap); err != nil {
		return err
	}

	return nil
}

func SyncInstance(sqleInstances map[uint] /*sqle inst id*/ *sqleModel.Instance) error {
	ids := []uint{}
	for _, instance := range sqleInstances {
		ids = append(ids, instance.ID)
	}

	// 从缓存中获取需要同步的CloudBeaver实例
	s := sqleModel.GetStorage()
	cbInstCacheSlice, err := s.GetCloudBeaverInstanceCacheBySQLEInstIDs(ids)
	if err != nil {
		return err
	}
	cbInstCacheMap := map[uint] /* sqle inst id*/ *sqleModel.CloudBeaverInstanceCache{}
	for _, cache := range cbInstCacheSlice {
		cbInstCacheMap[cache.SQLEInstanceID] = cache
	}

	// 找到需要同步的实例
	needAdd := []uint /*sqle inst id*/ {}
	needUpdate := []uint /*sqle inst id*/ {}
	for sqleInstID, sqleInst := range sqleInstances {
		cb, ok := cbInstCacheMap[sqleInstID]
		if !ok {
			needAdd = append(needAdd, sqleInstID)
		} else if cb.SQLEInstanceFingerprint != sqleInst.Fingerprint() {
			needUpdate = append(needUpdate, sqleInstID)
		}
	}

	if len(needAdd) == 0 && len(needUpdate) == 0 {
		return nil
	}

	// 获取管理员链接
	client, err := GetGQLClientWithRootUser()
	if err != nil {
		return err
	}

	l := log.NewEntry()

	// 同步实例信息
	for _, sqleInstID := range needAdd {
		project, _, err := s.GetProjectByID(sqleInstances[sqleInstID].ProjectId)
		if err != nil {
			l.Errorf("get instance %v project failed: %v", sqleInstID, err)
			project.Name = "unknown"
		}
		if err := AddCloudBeaverInstance(client, sqleInstances[sqleInstID], project); err != nil {
			l.Errorf("add instance %v to cloudbeaver failed: %v", sqleInstID, err)
		}
	}
	for _, sqleInstID := range needUpdate {
		project, _, err := s.GetProjectByID(sqleInstances[sqleInstID].ProjectId)
		if err != nil {
			l.Errorf("get instance %v project failed: %v", sqleInstID, err)
			project.Name = "unknown"
		}
		if err := UpdateCloudBeaverInstance(client, cbInstCacheMap[sqleInstID].CloudBeaverInstanceID, sqleInstances[sqleInstID], project); err != nil {
			l.Errorf("update instance %v to cloudbeaver failed: %v", sqleInstID, err)
		}
	}

	return nil

}

// AddCloudBeaverInstance 添加实例后会同步缓存
func AddCloudBeaverInstance(client *gqlClient.Client, sqleInst *sqleModel.Instance, project *sqleModel.Project) error {
	params, err := GenerateCloudBeaverInstanceParams(sqleInst, project)
	if err != nil {
		fmt.Println("Instances of this type are not currently supported:", sqleInst.DbType)
		// 不支持的类型跳过就好,没必要终端流程
		//nolint:nilerr
		return nil
	}
	// 添加实例
	req := gqlClient.NewRequest(QueryGQL.CreateConnectionQuery(), params)
	resp := struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}{}

	err = client.Run(context.TODO(), req, &resp)
	if err != nil {
		return err
	}

	// 同步缓存
	s := sqleModel.GetStorage()
	return s.Save(&sqleModel.CloudBeaverInstanceCache{
		CloudBeaverInstanceID:   resp.Connection.ID,
		SQLEInstanceID:          sqleInst.ID,
		SQLEInstanceFingerprint: sqleInst.Fingerprint(),
	})
}

// UpdateCloudBeaverInstance 更新完毕后会同步缓存
func UpdateCloudBeaverInstance(client *gqlClient.Client, cbInstID string, sqleInst *sqleModel.Instance, project *sqleModel.Project) error {

	params, err := GenerateCloudBeaverInstanceParams(sqleInst, project)
	if err != nil {
		fmt.Println("Instances of this type are not currently supported:", sqleInst.DbType)
		// 不支持的类型跳过就好,没必要终端流程
		//nolint:nilerr
		return nil
	}
	// 更新实例
	params["config"].(map[string]interface{})["connectionId"] = cbInstID
	req := gqlClient.NewRequest(QueryGQL.UpdateConnectionQuery(), params)
	resp := struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}{}

	err = client.Run(context.TODO(), req, &resp)
	if err != nil {
		return err
	}

	// 同步缓存
	s := sqleModel.GetStorage()
	return s.Save(&sqleModel.CloudBeaverInstanceCache{
		CloudBeaverInstanceID:   resp.Connection.ID,
		SQLEInstanceID:          sqleInst.ID,
		SQLEInstanceFingerprint: sqleInst.Fingerprint(),
	})
}

func SyncVisibleInstance(cbUserCache *sqleModel.CloudBeaverUserCache, sqleUser *sqleModel.User, sqleInstances map[uint] /*sqle inst id*/ *sqleModel.Instance) error {

	if cbUserCache.SQLEFingerprint != sqleUser.FingerPrint() {
		return fmt.Errorf("user information is not synchronized, unable to update instance information")
	}

	ids := []uint{}
	for _, instance := range sqleInstances {
		ids = append(ids, instance.ID)
	}

	// 从缓存中获取需要同步的CloudBeaver实例
	s := sqleModel.GetStorage()
	cbInstCacheSlice, err := s.GetCloudBeaverInstanceCacheBySQLEInstIDs(ids)
	if err != nil {
		return err
	}
	cbInstCacheMap := map[string] /* cb inst id*/ *sqleModel.CloudBeaverInstanceCache{}
	for _, cache := range cbInstCacheSlice {
		cbInstCacheMap[cache.CloudBeaverInstanceID] = cache
	}

	// 获取用户当前实例列表
	getConnResp := &struct {
		Connections []*struct {
			Id string `json:"id"`
		} `json:"connections"`
	}{}

	getConReq := gqlClient.NewRequest(QueryGQL.GetUserConnectionsQuery(), nil)

	client, err := GetGQLClient(cbUserCache.CloudBeaverUserID, sqleUser.Password)
	if err != nil {
		return err
	}

	err = client.Run(context.TODO(), getConReq, getConnResp)
	if err != nil {
		return err
	}

	// 判断是否需要同步权限
	if len(getConnResp.Connections) != len(cbInstCacheSlice) {
		return syncVisibleInstance(cbInstCacheSlice, cbUserCache.CloudBeaverUserID)
	}
	for _, connection := range getConnResp.Connections {
		if _, ok := cbInstCacheMap[connection.Id]; !ok {
			return syncVisibleInstance(cbInstCacheSlice, cbUserCache.CloudBeaverUserID)
		}
	}
	return nil

}

func syncVisibleInstance(cbInstCacheSlice []*sqleModel.CloudBeaverInstanceCache, cloudBeaverUserID string) error {
	cbInstIDs := []string{}
	for _, cache := range cbInstCacheSlice {
		cbInstIDs = append(cbInstIDs, cache.CloudBeaverInstanceID)
	}
	setConnReq := gqlClient.NewRequest(QueryGQL.SetUserConnectionsQuery(), map[string]interface{}{
		"userId":      cloudBeaverUserID,
		"connections": cbInstIDs,
	})
	rootClient, err := GetGQLClientWithRootUser()
	if err != nil {
		return err
	}
	err = rootClient.Run(context.TODO(), setConnReq, nil)
	return err
}

func generateCommonCloudBeaverConfigParams(sqleInst *sqleModel.Instance, project *sqleModel.Project) map[string]interface{} {
	return map[string]interface{}{
		"configurationType": "MANUAL",
		"name":              fmt.Sprintf("%v: %v", project.Name, sqleInst.Name),
		"template":          false,
		"host":              sqleInst.Host,
		"port":              sqleInst.Port,
		"databaseName":      nil,
		"description":       nil,
		"authModelId":       "native",
		"saveCredentials":   true,
		"credentials": map[string]interface{}{
			"userName":     sqleInst.User,
			"userPassword": sqleInst.Password,
		},
	}
}

func GenerateCloudBeaverInstanceParams(sqleInst *sqleModel.Instance, project *sqleModel.Project) (map[string]interface{}, error) {
	var err error
	config := generateCommonCloudBeaverConfigParams(sqleInst, project)

	switch sqleInst.DbType {
	case driverV2.DriverTypeMySQL:
		err = fillMySQLParams(config)
	case driverV2.DriverTypeTiDB:
		err = fillTiDBParams(config)
	case driverV2.DriverTypePostgreSQL:
		err = fillPGSQLParams(config)
	case driverV2.DriverTypeSQLServer:
		err = fillMSSQLParams(config)
	case driverV2.DriverTypeOracle:
		err = fillOracleParams(sqleInst, config)
	case driverV2.DriverTypeDB2:
		err = fillDB2Params(sqleInst, config)
	case driverV2.DriverTypeOceanBase:
		err = fillOceanBaseParams(sqleInst, config)
	default:
		return nil, fmt.Errorf("temporarily unsupported instance types")
	}

	resp := map[string]interface{}{
		"projectId": "g_GlobalConfiguration",
		"config":    config,
	}
	return resp, err
}

func fillMySQLParams(config map[string]interface{}) error {
	config["driverId"] = "mysql:mysql8"
	return nil
}

func fillTiDBParams(config map[string]interface{}) error {
	config["driverId"] = "mysql:tidb"
	return nil
}

func fillMSSQLParams(config map[string]interface{}) error {
	config["driverId"] = "sqlserver:microsoft"
	config["authModelId"] = "sqlserver_database"
	return nil
}

func fillPGSQLParams(config map[string]interface{}) error {
	config["driverId"] = "postgresql:postgres-jdbc"
	config["providerProperties"] = map[string]interface{}{
		"@dbeaver-show-non-default-db@": true,
		"@dbeaver-show-unavailable-db@": true,
		"@dbeaver-show-template-db@":    true,
	}
	return nil
}

func fillOracleParams(inst *sqleModel.Instance, config map[string]interface{}) error {
	serviceName := inst.AdditionalParams.GetParam("service_name")
	if serviceName == nil {
		return fmt.Errorf("the service name of oracle cannot be empty")
	}

	config["driverId"] = "oracle:oracle_thin"
	config["authModelId"] = "oracle_native"
	config["databaseName"] = serviceName.Value
	config["providerProperties"] = map[string]interface{}{
		"@dbeaver-sid-service@": "SID",
		"oracle.logon-as":       "Normal",
	}
	return nil
}

func fillDB2Params(inst *sqleModel.Instance, config map[string]interface{}) error {
	dbName := inst.AdditionalParams.GetParam("database_name")
	if dbName == nil {
		return fmt.Errorf("the database name of DB2 cannot be empty")
	}

	config["driverId"] = "db2:db2"
	config["databaseName"] = dbName.Value
	return nil
}

func fillOceanBaseParams(inst *sqleModel.Instance, config map[string]interface{}) error {
	tenant := inst.AdditionalParams.GetParam("tenant_name")
	if tenant == nil {
		return fmt.Errorf("the tenant name of oceanbase cannot be empty")
	}

	config["driverId"] = "oceanbase:alipay_oceanbase"
	config["authModelId"] = "oceanbase_native"
	config["credentials"].(map[string]interface{})["userName"] = fmt.Sprintf("%v@%v", inst.User, tenant)
	return nil
}
