package application

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"flamingo.me/flamingo/v3/framework/flamingo"
	"flamingo.me/flamingo/v3/framework/web"
	"flamingo.me/form/domain"
)

type (
	// formHandlerImpl as actual implementation of FormHandler interface
	formHandlerImpl struct {
		formDataProvider         domain.FormDataProvider
		formDataDecoder          domain.FormDataDecoder
		formDataValidator        domain.FormDataValidator
		defaultFormDataProvider  domain.DefaultFormDataProvider
		defaultFormDataDecoder   domain.DefaultFormDataDecoder
		defaultFormDataValidator domain.DefaultFormDataValidator
		formExtensions           map[string]domain.FormExtension
		validatorProvider        domain.ValidatorProvider
		logger                   flamingo.Logger
	}
)

var _ domain.FormHandler = &formHandlerImpl{}

// HandleForm as method for returning Form instance with state depending on fact if there was form submission or not, via POST request
func (h *formHandlerImpl) HandleForm(ctx context.Context, req *web.Request) (*domain.Form, error) {
	submitted := req.Request().Method == http.MethodPost

	form, err := h.buildForm(ctx, req, submitted)
	if err != nil {
		return nil, err
	}

	if submitted {
		return h.handleSubmittedForm(ctx, req, form, http.MethodPost)
	}

	return form, nil
}

// HandleUnsubmittedForm as method for returning Form instance which is not submitted
func (h *formHandlerImpl) HandleUnsubmittedForm(ctx context.Context, req *web.Request) (*domain.Form, error) {
	form, err := h.buildForm(ctx, req, false)
	if err != nil {
		return nil, err
	}

	err = h.processExtensions(ctx, req, url.Values{}, form)
	if err != nil {
		h.getLogger("formExtensions").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	}

	return form, nil
}

// HandleSubmittedForm as method for returning Form instance which is submitted via POST request
func (h *formHandlerImpl) HandleSubmittedForm(ctx context.Context, req *web.Request) (*domain.Form, error) {
	form, err := h.buildForm(ctx, req, true)
	if err != nil {
		return nil, err
	}

	return h.handleSubmittedForm(ctx, req, form, http.MethodPost)
}

// HandleSubmittedGETForm as method for returning Form instance which is submitted via GET request
func (h *formHandlerImpl) HandleSubmittedGETForm(ctx context.Context, req *web.Request) (*domain.Form, error) {
	form, err := h.buildForm(ctx, req, true)
	if err != nil {
		return nil, err
	}

	return h.handleSubmittedForm(ctx, req, form, http.MethodGet)
}

// buildForm as method for creating new instance of Form domain
func (h *formHandlerImpl) buildForm(ctx context.Context, req *web.Request, submitted bool) (*domain.Form, error) {
	validationRules, err := h.collectFormExtensionValidationRules(ctx, req)
	if err != nil {
		h.getLogger("formExtensions").Error(err.Error())
		return nil, err
	}

	formData, err := h.getFormData(ctx, req, h.formDataProvider)
	if err != nil {
		h.getLogger("formBuilding").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	}

	mainValidationRules := h.extractValidationRules(formData)
	validationRules = h.mergeValidationRules(validationRules, mainValidationRules)
	form := domain.NewForm(submitted, validationRules)
	form.Data = formData

	return &form, nil
}

// collectFormExtensionValidationRules collects validation rules from all form extensions defined for handler and delivers them as a single map
func (h *formHandlerImpl) collectFormExtensionValidationRules(ctx context.Context, req *web.Request) (map[string][]domain.ValidationRule, error) {
	validationRules := map[string][]domain.ValidationRule{}
	for _, formExtension := range h.formExtensions {
		var formDataProvider domain.FormDataProvider
		if provider, ok := formExtension.(domain.FormDataProvider); ok {
			formDataProvider = provider
		}
		extensionFormData, err := h.getFormData(ctx, req, formDataProvider)
		if err != nil {
			return nil, domain.NewFormError(err.Error())
		}
		extensionValidationRules := h.extractValidationRules(extensionFormData)
		validationRules = h.mergeValidationRules(validationRules, extensionValidationRules)
	}
	return validationRules, nil
}

// handleSubmittedForm as method for processing
func (h *formHandlerImpl) handleSubmittedForm(ctx context.Context, req *web.Request, form *domain.Form, method string) (*domain.Form, error) {
	values, err := h.getURLValues(req, method)
	if err != nil {
		h.getLogger("postValueProcessing").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	}

	formData, err := h.decode(ctx, req, *values, form.Data, h.formDataDecoder)
	if err != nil {
		h.getLogger("formDecoding").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	}
	form.Data = formData

	validationInfo, err := h.validate(ctx, req, h.validatorProvider, formData, h.formDataValidator)
	if err != nil {
		h.getLogger("formValidation").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	} else if validationInfo == nil {
		validationInfo = &domain.ValidationInfo{}
	}
	form.ValidationInfo = *validationInfo

	err = h.processExtensions(ctx, req, *values, form)
	if err != nil {
		h.getLogger("formExtensions").Error(err.Error())
		return nil, domain.NewFormError(err.Error())
	}

	return form, nil
}

// mergeValidationRules merges two validation rules maps into one
func (h *formHandlerImpl) mergeValidationRules(first map[string][]domain.ValidationRule, second map[string][]domain.ValidationRule) map[string][]domain.ValidationRule {
	for k, v := range second {
		first[k] = v
	}
	return first
}

// extractValidationRules as method for extracting form fields validation rules
func (h *formHandlerImpl) extractValidationRules(formData interface{}) map[string][]domain.ValidationRule {
	validationRules := map[string][]domain.ValidationRule{}

	if formData == nil {
		return validationRules
	}

	valueOf := reflect.Indirect(reflect.ValueOf(formData))
	typeOf := valueOf.Type()

	if typeOf.Kind() != reflect.Struct {
		return validationRules
	}

	for i := 0; i < typeOf.NumField(); i++ {
		fieldValue := valueOf.Field(i)
		if fieldValue.Type().Kind() == reflect.Ptr && fieldValue.Type().Elem().Kind() == reflect.Struct {
			fieldValue = reflect.Zero(fieldValue.Type().Elem())
		}

		fieldType := typeOf.Field(i)

		name := fieldType.Tag.Get("form")
		if name == "-" {
			continue
		}

		if name == "" {
			name = fieldType.Name
		}

		if fieldValue.Kind() == reflect.Struct {
			subRules := h.extractValidationRules(fieldValue.Interface())
			for k, v := range subRules {
				key := fmt.Sprintf("%s.%s", name, k)
				validationRules[key] = v
			}

			continue
		}

		validationTag := fieldType.Tag.Get("validate")
		if validationTag == "" {
			continue
		}

		tags := strings.Split(validationTag, ",")
		for _, tag := range tags {
			values := strings.Split(tag, "=")
			if len(values) == 0 {
				continue
			}
			if values[0] == "omitempty" || values[0] == "" {
				continue
			}

			validationRule := domain.ValidationRule{
				Name: values[0],
			}
			if len(values) > 1 {
				validationRule.Value = values[1]
			}

			validationRules[name] = append(validationRules[name], validationRule)
		}
	}

	return validationRules
}

// getPostValues as method for extracting http request body
func (h *formHandlerImpl) getURLValues(r *web.Request, method string) (*url.Values, error) {
	if method == http.MethodGet {
		values := r.Request().URL.Query()
		return &values, nil
	}

	err := r.Request().ParseForm()
	if err != nil {
		return nil, err
	}

	return &r.Request().Form, nil
}

// processExtensions as method for processing list of form extensions
func (h *formHandlerImpl) processExtensions(ctx context.Context, req *web.Request, values url.Values, form *domain.Form) error {
	for name, formExtension := range h.formExtensions {
		err := h.processExtension(ctx, req, values, name, formExtension, form)
		if err != nil {
			return err
		}
	}

	return nil
}

// processExtension as method for processing single form extensions
func (h *formHandlerImpl) processExtension(ctx context.Context, req *web.Request, values url.Values, name string, formExtension interface{}, form *domain.Form) error {
	var formData interface{}
	var err error

	// checks if form extension is defined as form data provider
	// if it's not, it passes nil, which means that default form data provider will be used
	var formDataProvider domain.FormDataProvider
	if provider, ok := formExtension.(domain.FormDataProvider); ok {
		formDataProvider = provider
	}
	formData, err = h.getFormData(ctx, req, formDataProvider)
	if err != nil {
		return err
	}

	if form.FormExtensionsData == nil {
		form.FormExtensionsData = map[string]interface{}{}
	}

	// at this point decoded data is added to map of form extension data
	form.FormExtensionsData[name] = formData

	if !form.IsSubmitted() {
		return nil
	}

	// checks if form extension is defined as form data decoder
	// if it's not, it passes nil, which means that default form data decoder will be used
	var formDataDecoder domain.FormDataDecoder
	if decoder, ok := formExtension.(domain.FormDataDecoder); ok {
		formDataDecoder = decoder
	}
	formData, err = h.decode(ctx, req, values, formData, formDataDecoder)
	if err != nil {
		return err
	}

	// at this point decoded data is added to map of form extension data
	form.FormExtensionsData[name] = formData

	// checks if form extension is defined as form data validator
	// if it's not, it passes nil, which means that default form data validator will be used
	var formDataValidator domain.FormDataValidator
	if validator, ok := formExtension.(domain.FormDataValidator); ok {
		formDataValidator = validator
	}
	validationInfo, err := h.validate(ctx, req, h.validatorProvider, formData, formDataValidator)
	if err != nil {
		return err
	}

	// form validation errors from form extension is attached
	form.ValidationInfo.AppendGeneralErrors(validationInfo.GetGeneralErrors())
	form.ValidationInfo.AppendFieldErrors(validationInfo.GetErrorsForAllFields())

	return nil
}

// formHandlerImpl returns flamingo logger instance with defined fields for error logging
func (h *formHandlerImpl) getLogger(value string) flamingo.Logger {
	return h.logger.WithField("FormHandler", value)
}

// getFormData calls GetFormData from instance of domain.FormDataProvider if it's defined, otherwise it calls it from default domain.FormDataProvider
func (h *formHandlerImpl) getFormData(ctx context.Context, req *web.Request, formDataProvider domain.FormDataProvider) (interface{}, error) {
	if formDataProvider == nil {
		formDataProvider = h.defaultFormDataProvider
	}

	return formDataProvider.GetFormData(ctx, req)
}

// decode calls Decode from instance of domain.FormDataDecoder if it's defined, otherwise it calls it from default domain.FormDataDecoder
func (h *formHandlerImpl) decode(ctx context.Context, req *web.Request, values url.Values, formData interface{}, formDataDecoder domain.FormDataDecoder) (interface{}, error) {
	if formDataDecoder == nil {
		formDataDecoder = h.defaultFormDataDecoder
	}

	return formDataDecoder.Decode(ctx, req, values, formData)
}

// validate calls Validate from instance of domain.FormDataValidator if it's defined, otherwise it calls it from default domain.FormDataValidator
func (h *formHandlerImpl) validate(ctx context.Context, req *web.Request, validatorProvider domain.ValidatorProvider, formData interface{}, formDataValidator domain.FormDataValidator) (*domain.ValidationInfo, error) {
	if formDataValidator == nil {
		formDataValidator = h.defaultFormDataValidator
	}

	return formDataValidator.Validate(ctx, req, validatorProvider, formData)
}
