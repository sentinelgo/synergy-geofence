package errors

import "fmt"

type CustomError struct {
	Code  string     `json:"code"`
	Input ErrorInput `json:"input"`
}

func (x *CustomError) Error() string {
	return fmt.Sprintf("Code: %s, Input: %s", x.Code, x.Input)
}

func NewError(code string, input ErrorInput) *CustomError {
	if len(code) == 0 && input == "" {
		return nil
	}
	return &CustomError{Code: code, Input: input}
}

func GetError(code string, input ErrorInput) error {
	if len(code) == 0 && input == "" {
		return nil
	}
	return &CustomError{Code: code, Input: input}
}

const (
	ErrMissingFields   = "ERR_EMPTY"
	ErrMismatch        = "ERR_MISMATCH"
	ErrGeofenceFind    = "ERR_DB_NOTFOUND"
	ErrGeofenceUpdate  = "ERR_DB_UPDATE"
	ErrGeofenceCreate  = "ERR_DB_SAVE"
	ErrGeofenceDelete  = "ERR_DB_DELETE"
	ErrBindJson        = "ERR_BIND_PAYLOAD"
	ErrUUIDParse       = "ERR_UUID_FORMAT"
	ErrConvertData     = "ERR_CONVERT_DATA"
	ErrOptimisticLock  = "ERR_OPTIMISTIC_LOCK"
	ErrInvalidGeometry = "ERR_INVALID_GEOMETRY"
	ErrInvalidQuery    = "ERR_INVALID_QUERY"
	ErrDuplicate       = "ERR_DUPLICATE"
	ErrInvalidName     = "ERR_INVALID_NAME"
	ErrCommon          = "ERR_COMMON"
	ErrAuth            = "ERR_AUTH"
)

type ErrorInput string

const (
	AgencyId    ErrorInput = "agency_id"
	ClientId    ErrorInput = "client_id"
	GeofenceId  ErrorInput = "geofence_id"
	Geofence    ErrorInput = "geofence"
	GeofenceReq ErrorInput = "geofence_req"
	Coordinates ErrorInput = "coordinates"
	Lookup      ErrorInput = "lookup"
	Pagination  ErrorInput = "pagination"
	Name        ErrorInput = "name"
	UserId      ErrorInput = "user_id"
	Claims      ErrorInput = "claims"
)
