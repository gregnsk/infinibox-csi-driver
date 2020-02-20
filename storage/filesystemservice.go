package storage

import (
	"errors"
	"fmt"
	"infinibox-csi-driver/api"
	"path"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

//treeq constants
const (
	PROVISIONTYPE          = "provision_type"
	PVSIZE                 = "pv_size"
	MAXTREEQSPERFILESYSTEM = "max_treeqs_per_filesystem"
	MAXFILESYSTEMS         = "max_filesystems"
	MAXFILESYSTEMSIZE      = "max_filesystem_size"
	FSPREFIX               = "fs_prefix"
	PVPREFIX               = "pv_prefix"
	UNIXPERMISSION         = "nfs_unix_permissions"

	//Treeq count
	TREEQCOUNT = "host.k8s.treeqs"
)

// service type
const (
	NFSTREEQ = "nfs_treeq"
	NFS      = "nfs"
)

//FilesystemService file system services
type FilesystemService struct {
	uniqueID  int64
	configmap map[string]string // values from storage class
	pVName    string
	capacity  int64

	fileSystemID int64
	exportpath   string
	exportID     int64
	exportBlock  string
	ipAddress    string

	cs       commonservice
	poolID   int64
	treeqCnt int

	treeqVolume map[string]string
}

func getFilesystemService(serviceType string, c commonservice) *FilesystemService {
	if NFSTREEQ == serviceType {
		return &FilesystemService{
			cs:          c,
			treeqVolume: make(map[string]string),
		}
	}
	return nil
}

func (filesystem *FilesystemService) getExpectedFileSystemID() (filesys *api.FileSystem, err error) {
	poolID, err := filesystem.cs.api.GetStoragePoolIDByName(filesystem.configmap["pool_name"])
	if err != nil {
		log.Errorf("fail to get poolID from poolName %s", filesystem.configmap["pool_name"])
		return
	}
	filesystem.poolID = poolID
	page := 1
	maxFileSystemSize, err := filesystem.maxFileSize()
	if err != nil {
		log.Error(err)
		return
	}
	for {
		fsMetaData, poolErr := filesystem.cs.api.GetFileSystemsByPoolID(poolID, page)
		if poolErr != nil {
			log.Errorf("fail to get filesystems from poolID %d and page no %d error %v", poolID, page, err)
			err = errors.New("fail to get filesystems from poolName " + filesystem.configmap["pool_name"])
			return
		}
		if fsMetaData != nil && len(fsMetaData.FileSystemArry) == 0 {
			log.Debugf("NO filesystem found.filesystem array is empty")
			return
		}
		for _, fs := range fsMetaData.FileSystemArry {
			if fs.Size+filesystem.capacity < maxFileSystemSize {
				treeqCnt, treeqCnterr := filesystem.cs.api.GetFilesytemTreeqCount(fs.ID)
				if treeqCnterr != nil {
					log.Errorf("fail to get treeq count of filesystemID %d error %v", fs.ID, err)
					err = errors.New("fail to get treeq count of filesystemID " + strconv.FormatInt(fs.ID, 10))
					return
				}
				if treeqCnt < filesystem.getAllowedCount(MAXTREEQSPERFILESYSTEM) {
					filesystem.treeqCnt = treeqCnt
					log.Debugf("filesystem found to create treeQ,filesystemID %d", fs.ID)
					exportErr := filesystem.getExportPath(fs.ID) //fetch export path and set to filesystem exportPath
					if exportErr != nil {
						err = exportErr
					}
					filesys = &fs
					return
				}
			}
		} //inner for loop closed
		if fsMetaData.Filemetadata.PagesTotal == fsMetaData.Filemetadata.Page {
			break
		}
		page++ //check the file system on next page
	} //outer for loop closed
	log.Debugf("NO filesystem found to create treeQ")
	return
}

//CreateNFSVolume create volumne method
func (filesystem *FilesystemService) CreateNFSVolume() (err error) {
	defer func() {
		if res := recover(); res != nil {
			err = errors.New("error while creating treeq method " + fmt.Sprint(res))
		}
	}()

	filesys, err := filesystem.getExpectedFileSystemID()
	if err != nil {
		log.Errorf("fail to get filesystem to add treeq %v", err)
		return
	}
	var filesystemID int64
	if filesys == nil { // if pool is empty or no file system found to createTreeq
		err = filesystem.createFileSystem()
		if err != nil {
			log.Errorf("fail to create fileSystem %v", err)
			return err
		}
		err = filesystem.createExportPathAndAddMetadata()
		if err != nil {
			log.Errorf("fail to create export and metadata %v", err)
			return err
		}
		filesystemID = filesystem.fileSystemID
	} else {
		filesystemID = filesys.ID
	}
	//create treeq
	treeqResponse, createTreeqerr := filesystem.cs.api.CreateTreeq(filesystemID, filesystem.getTreeParameters())
	if createTreeqerr != nil {
		log.Errorf("fail to create treeq  %s error %v", filesystem.pVName, err)
		err = errors.New("fail to Create Treeq")
		return
	}

	filesystem.treeqVolume["ID"] = strconv.FormatInt(treeqResponse.ID, 10)
	filesystem.treeqVolume["ipAddress"] = filesystem.ipAddress
	filesystem.treeqVolume["volumePath"] = path.Join(filesystem.exportpath, treeqResponse.Path)

	//if AttachMetadataToObject - fail to add metadata then delete the created treeq
	defer func() {
		if res := recover(); res != nil {
			err = errors.New("error while update metadata" + fmt.Sprint(res))
		}
		if err != nil && filesystem.fileSystemID != 0 {
			log.Infof("Seemes to be some problem reverting treeq: %s", filesystem.pVName)
			filesystem.cs.api.DeleteTreeq(filesystem.fileSystemID, treeqResponse.ID)
		}
	}()

	//update treeq count in metadta //AttachMetadataToObject()
	metadataParamter := make(map[string]interface{})
	metadataParamter[TREEQCOUNT] = filesystem.treeqCnt + 1
	_, metadataErr := filesystem.cs.api.AttachMetadataToObject(filesystemID, metadataParamter)
	if metadataErr != nil {
		log.Errorf("fail to increment treeq count %s error %v", TREEQCOUNT, err)
		err = errors.New("fail to increment treeq count as metadata")
		return
	}

	//if UpdateFilesystem - is fail then descrement the tree count from metadata
	defer func() {
		if res := recover(); res != nil {
			err = errors.New("error while update file size" + fmt.Sprint(res))
		}
		if err != nil && filesystemID != 0 {
			log.Infof("Seemes to be some problem reverting treeqcount")
			filesystem.cs.api.AttachMetadataToObject(filesystemID, metadataParamter)
		}
	}()

	// if new file system is created ,while creating the treeq, then not need to update size
	if filesys != nil {
		var updateFileSys api.FileSystem
		updateFileSys.Size = filesys.Size + filesystem.capacity
		_, updateFileSizeErr := filesystem.cs.api.UpdateFilesystem(filesystemID, updateFileSys)
		if updateFileSizeErr != nil {
			log.Errorf("fail to update File Size %v", err)
			err = errors.New("fail to update files size")
			return
		}
	}

	return
}

func (filesystem *FilesystemService) createExportPathAndAddMetadata() (err error) {
	defer func() {
		if res := recover(); res != nil {
			err = errors.New("error while export directory" + fmt.Sprint(res))
		}
		if err != nil && filesystem.fileSystemID != 0 {
			log.Infof("Seemes to be some problem reverting filesystem: %s", filesystem.pVName)
			filesystem.cs.api.DeleteFileSystem(filesystem.fileSystemID)
		}
	}()

	err = filesystem.createExportPath()
	if err != nil {
		log.Errorf("fail to export path %v", err)
		return
	}
	log.Debugf("export path created for filesystem: %s", filesystem.pVName)

	defer func() {
		if res := recover(); res != nil {
			err = errors.New("error while AttachMetadata directory" + fmt.Sprint(res))
		}
		if err != nil && filesystem.exportID != 0 {
			log.Infoln("Seemes to be some problem reverting created export id:", filesystem.exportID)
			filesystem.cs.api.DeleteExportPath(filesystem.exportID)
		}
	}()
	metadata := make(map[string]interface{})
	metadata["host.k8s.pvname"] = filesystem.pVName
	metadata["filesystem_type"] = ""

	_, err = filesystem.cs.api.AttachMetadataToObject(filesystem.fileSystemID, metadata)
	if err != nil {
		log.Errorf("fail to attach metadata for fileSystem : %s", filesystem.pVName)
		log.Errorf("error to attach metadata %v", err)
		return
	}
	log.Debugf("metadata attached successfully for filesystem %s", filesystem.pVName)
	return
}

func (filesystem *FilesystemService) createFileSystem() (err error) {
	fileSystemCnt, err := filesystem.cs.api.GetFileSystemCount()
	if err != nil {
		log.Errorf("fail to get the filesystem count from Ibox %v", err)
		return
	}
	if fileSystemCnt >= filesystem.getAllowedCount(MAXFILESYSTEMS) {
		log.Debugf("Max filesystem allowed on Ibox %v", filesystem.getAllowedCount(MAXFILESYSTEMS))
		log.Debugf("Current filesystem count on Ibox %v", fileSystemCnt)
		log.Errorf("Ibox not allowed to create new file system")
		err = errors.New("Ibox not allowed to create new file system")
		return
	}
	ssdEnabled := filesystem.configmap["ssd_enabled"]
	if ssdEnabled == "" {
		ssdEnabled = fmt.Sprint(false)
	}
	ssd, _ := strconv.ParseBool(ssdEnabled)
	mapRequest := make(map[string]interface{})
	mapRequest["pool_id"] = filesystem.poolID
	mapRequest["name"] = filesystem.pVName
	mapRequest["ssd_enabled"] = ssd
	mapRequest["provtype"] = strings.ToUpper(filesystem.configmap["provision_type"])
	mapRequest["size"] = filesystem.capacity
	fileSystem, err := filesystem.cs.api.CreateFilesystem(mapRequest)
	if err != nil {
		log.Errorf("fail to create filesystem %s", filesystem.pVName)
		return
	}
	filesystem.fileSystemID = fileSystem.ID
	log.Debugf("filesystem Created %s", filesystem.pVName)
	return
}

func (filesystem *FilesystemService) createExportPath() (err error) {
	permissionsMapArray, err := getPermission(filesystem.configmap["nfs_export_permissions"])
	if err != nil {
		return
	}
	var permissionsput []map[string]interface{}
	for _, pass := range permissionsMapArray {
		access := pass["access"].(string)
		var rootsq bool
		_, ok := pass["no_root_squash"].(string)
		if ok {
			rootsq, err = strconv.ParseBool(pass["no_root_squash"].(string))
			if err != nil {
				log.Debug("fail to cast no_root_squash value in export permission . setting default value 'true' ")
				rootsq = true
			}
		} else {
			rootsq = pass["no_root_squash"].(bool)
		}
		client := pass["client"].(string)
		permissionsput = append(permissionsput, map[string]interface{}{"access": access, "no_root_squash": rootsq, "client": client})
	}
	var exportFileSystem api.ExportFileSys
	exportFileSystem.FilesystemID = filesystem.fileSystemID
	exportFileSystem.Transport_protocols = "TCP"
	exportFileSystem.Privileged_port = true
	exportFileSystem.Export_path = filesystem.exportpath
	exportFileSystem.Permissionsput = append(exportFileSystem.Permissionsput, permissionsput...)
	exportResp, err := filesystem.cs.api.ExportFileSystem(exportFileSystem)
	if err != nil {
		log.Errorf("fail to create export path of filesystem %s", filesystem.pVName)
		return
	}
	filesystem.exportID = exportResp.ID
	filesystem.exportBlock = exportResp.ExportPath
	return
}

func (filesystem *FilesystemService) validateTreeqParameters(config map[string]string) (bool, map[string]string) {
	compulsaryFields := []string{"pool_name", "nfs_networkspace"}
	validationStatus := true
	validationStatusMap := make(map[string]string)
	for _, param := range compulsaryFields {
		if config[param] == "" {
			validationStatusMap[param] = param + " value missing"
			validationStatus = false
		}
	}
	log.Debug("parameter Validation completed")
	return validationStatus, validationStatusMap
}

func getDefaultValues() map[string]string {
	defaultConfigMap := make(map[string]string)
	defaultConfigMap[PROVISIONTYPE] = "thin"
	defaultConfigMap[PVSIZE] = "1gib"
	defaultConfigMap[MAXTREEQSPERFILESYSTEM] = "1000"
	defaultConfigMap[MAXFILESYSTEMS] = "1000"
	defaultConfigMap[MAXFILESYSTEMSIZE] = "100gib"
	defaultConfigMap[UNIXPERMISSION] = "750"
	return defaultConfigMap
}

func (filesystem *FilesystemService) getAllowedCount(key string) int {
	var allowedCnt int = 0
	if _, ok := filesystem.configmap[key]; ok {
		allowedCnt, err := strconv.Atoi(filesystem.configmap[key])
		if err == nil {
			return allowedCnt
		}
	}
	defaultConfigMap := getDefaultValues()
	val := defaultConfigMap[key]
	allowedCnt, _ = strconv.Atoi(val)
	return allowedCnt

}

func (filesystem *FilesystemService) maxFileSize() (sizeInByte int64, err error) {
	if maxfilesize, ok := filesystem.configmap[MAXFILESYSTEMSIZE]; ok {
		sizeInByte, err = convertToByte(maxfilesize)
		if err != nil {
			log.Errorf("fail to convert MAXFILESYSTEMSIZE %s to byte", maxfilesize)
		}
		return
	}
	defaultSize := getDefaultValues()[MAXFILESYSTEMSIZE]
	sizeInByte, err = convertToByte(defaultSize)
	return
}

func convertToByte(size string) (bytes int64, err error) {
	sizeUnits := make(map[string]int64)
	sizeUnits["gib"] = gib
	sizeUnits["tib"] = tib
	for key, unit := range sizeUnits {
		if strings.Contains(size, key) {
			arg := strings.Split(size, key)
			sizeUnit, errConvert := strconv.ParseInt(arg[0], 10, 64)
			if errConvert != nil {
				log.Errorf("fail to convert the %s to bytes", size)
				return
			}
			bytes = sizeUnit * unit
			return
		}
	}
	err = errors.New("Unexpected maxfilesystemsize .Expected size formate shoude be in formate of gib,tib")
	return
}

func (filesystem *FilesystemService) setExportpathPrefix() {
	if prefix, ok := filesystem.configmap[FSPREFIX]; ok {
		filesystem.exportpath = path.Join(prefix, filesystem.pVName)
	}
}

func (filesystem *FilesystemService) getTreeParameters() map[string]interface{} {
	treeqParameter := make(map[string]interface{})
	treeqParameter["path"] = "/" + filesystem.pVName
	treeqParameter["name"] = filesystem.pVName
	treeqParameter["hard_capacity"] = filesystem.capacity
	treeqParameter["mode"] = filesystem.getTreeModePermission()
	return treeqParameter
}

func (filesystem *FilesystemService) getTreeModePermission() string {
	if unixPermission, ok := filesystem.configmap[UNIXPERMISSION]; ok {
		return unixPermission
	}
	values := getDefaultValues()
	return values[UNIXPERMISSION]
}

func (filesystem *FilesystemService) getExportPath(filesystemID int64) error {
	exportResponse, exportErr := filesystem.cs.api.GetExportByFileSystem(filesystemID)
	if exportErr != nil {
		log.Errorf("fail to create export path of filesystem %d", filesystemID)
		return exportErr
	}
	for _, export := range *exportResponse {
		filesystem.exportpath = export.ExportPath
		break
	}
	return nil
}