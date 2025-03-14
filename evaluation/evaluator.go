package evaluation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/harness/ff-golang-server-sdk/logger"

	"github.com/harness/ff-golang-server-sdk/rest"
)

const (
	oneHundred = 100

	segmentMatchOperator   = "segmentMatch"
	matchOperator          = "match"
	inOperator             = "in"
	equalOperator          = "equal"
	gtOperator             = "gt"
	startsWithOperator     = "starts_with"
	endsWithOperator       = "ends_with"
	containsOperator       = "contains"
	equalSensitiveOperator = "equal_sensitive"
)

// Query provides methods for segment and flag retrieval
type Query interface {
	GetSegment(identifier string) (rest.Segment, error)
	GetFlag(identifier string) (rest.FeatureConfig, error)
}

// PostEvalData holds information for post evaluation processing
type PostEvalData struct {
	FeatureConfig *rest.FeatureConfig
	Target        *Target
	Variation     *rest.Variation
}

// PostEvaluateCallback interface can be used for advanced processing
// of evaluated data
type PostEvaluateCallback interface {
	PostEvaluateProcessor(data *PostEvalData)
}

// Evaluator engine evaluates flag from provided query
type Evaluator struct {
	query            Query
	postEvalCallback PostEvaluateCallback
	logger           logger.Logger
}

// NewEvaluator constructs evaluator with query instance
func NewEvaluator(query Query, postEvalCallback PostEvaluateCallback, logger logger.Logger) (*Evaluator, error) {
	if query == nil {
		return nil, ErrQueryProviderMissing
	}
	return &Evaluator{
		logger:           logger,
		query:            query,
		postEvalCallback: postEvalCallback,
	}, nil
}

func (e Evaluator) evaluateClause(clause *rest.Clause, target *Target) bool {
	if clause == nil {
		return false
	}

	values := clause.Values
	if len(values) == 0 {
		return false
	}
	value := values[0]

	operator := clause.Op
	if operator == "" {
		return false
	}

	attrValue := getAttrValue(target, clause.Attribute)
	if operator != segmentMatchOperator && !attrValue.IsValid() {
		return false
	}

	object := ""
	switch attrValue.Kind() {
	case reflect.Int, reflect.Int64:
		object = strconv.FormatInt(attrValue.Int(), 10)
	case reflect.Bool:
		object = strconv.FormatBool(attrValue.Bool())
	case reflect.String:
		object = attrValue.String()
	case reflect.Array, reflect.Chan, reflect.Complex128, reflect.Complex64, reflect.Func, reflect.Interface,
		reflect.Invalid, reflect.Ptr, reflect.Slice, reflect.Struct, reflect.Uintptr, reflect.UnsafePointer,
		reflect.Float32, reflect.Float64, reflect.Int16, reflect.Int32, reflect.Int8, reflect.Map, reflect.Uint,
		reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint8:
		object = fmt.Sprintf("%v", object)
	default:
		// Use string formatting as last ditch effort for any unexpected values
		object = fmt.Sprintf("%v", object)
	}

	switch operator {
	case startsWithOperator:
		return strings.HasPrefix(object, value)
	case endsWithOperator:
		return strings.HasSuffix(object, value)
	case matchOperator:
		found, err := regexp.MatchString(value, object)
		if err != nil || !found {
			return false
		}
		return true
	case containsOperator:
		return strings.Contains(object, value)
	case equalOperator:
		return strings.EqualFold(object, value)
	case equalSensitiveOperator:
		return object == value
	case inOperator:
		for _, val := range values {
			if val == object {
				return true
			}
		}
		return false
	case gtOperator:
		return object > value
	case segmentMatchOperator:
		return e.isTargetIncludedOrExcludedInSegment(values, target)
	default:
		return false
	}
}

func (e Evaluator) evaluateClauses(clauses []rest.Clause, target *Target) bool {
	for i := range clauses {
		if !e.evaluateClause(&clauses[i], target) {
			return false
		}
	}
	return true
}

func (e Evaluator) evaluateRule(servingRule *rest.ServingRule, target *Target) bool {
	return e.evaluateClauses(servingRule.Clauses, target)
}

func (e Evaluator) evaluateRules(servingRules []rest.ServingRule, target *Target) string {
	if target == nil || servingRules == nil {
		return ""
	}

	sort.SliceStable(servingRules, func(i, j int) bool {
		return servingRules[i].Priority < servingRules[j].Priority
	})
	for i := range servingRules {
		rule := servingRules[i]
		// if evaluation is false just continue to next rule
		if !e.evaluateRule(&rule, target) {
			continue
		}

		// rule matched, check if there is distribution
		if rule.Serve.Distribution != nil {
			return evaluateDistribution(rule.Serve.Distribution, target)
		}

		// rule matched, here must be variation if distribution is undefined or null
		if rule.Serve.Variation != nil {
			return *rule.Serve.Variation
		}
	}
	return ""
}

func (e Evaluator) evaluateVariationMap(variationsMap []rest.VariationMap, target *Target) string {
	if variationsMap == nil || target == nil {
		return ""
	}

	for _, variationMap := range variationsMap {
		if variationMap.Targets != nil {
			for _, t := range *variationMap.Targets {
				if *t.Identifier != "" && *t.Identifier == target.Identifier {
					return variationMap.Variation
				}
			}
		}

		segmentIdentifiers := variationMap.TargetSegments
		if segmentIdentifiers != nil && e.isTargetIncludedOrExcludedInSegment(*segmentIdentifiers, target) {
			return variationMap.Variation
		}
	}
	return ""
}

func (e Evaluator) evaluateFlag(fc rest.FeatureConfig, target *Target) (rest.Variation, error) {
	var variation = fc.OffVariation
	if fc.State == rest.FeatureStateOn {
		variation = ""
		if fc.VariationToTargetMap != nil {
			variation = e.evaluateVariationMap(*fc.VariationToTargetMap, target)
		}
		if variation == "" && fc.Rules != nil {
			variation = e.evaluateRules(*fc.Rules, target)
		}
		if variation == "" {
			variation = evaluateDistribution(fc.DefaultServe.Distribution, target)
		}
		if variation == "" && fc.DefaultServe.Variation != nil {
			variation = *fc.DefaultServe.Variation
		}
	}

	if variation != "" {
		return findVariation(fc.Variations, variation)
	}
	return rest.Variation{}, fmt.Errorf("%w: %s", ErrEvaluationFlag, fc.Feature)
}

func (e Evaluator) isTargetIncludedOrExcludedInSegment(segmentList []string, target *Target) bool {
	if segmentList == nil {
		return false
	}
	for _, segmentIdentifier := range segmentList {
		segment, err := e.query.GetSegment(segmentIdentifier)
		if err != nil {
			return false
		}
		// Should Target be excluded - if in excluded list we return false
		if segment.Excluded != nil && isTargetInList(target, *segment.Excluded) {
			e.logger.Debugf("Target %s excluded from segment %s via exclude list", target.Name, segment.Name)
			return false
		}

		// Should Target be included - if in included list we return true
		if segment.Included != nil && isTargetInList(target, *segment.Included) {
			e.logger.Debugf(
				"Target %s included in segment %s via include list",
				target.Name,
				segment.Name)
			return true
		}

		// Should Target be included via segment rules
		rules := segment.Rules
		if rules != nil && e.evaluateClauses(*rules, target) {
			e.logger.Debugf(
				"Target %s included in segment %s via rules", target.Name, segment.Name)
			return true
		}
	}
	return false
}

func (e Evaluator) checkPreRequisite(fc *rest.FeatureConfig, target *Target) (bool, error) {
	if e.query == nil {
		e.logger.Errorf(ErrQueryProviderMissing.Error())
		return true, ErrQueryProviderMissing
	}
	prerequisites := fc.Prerequisites
	if prerequisites != nil {
		e.logger.Debugf(
			"Checking pre requisites %v of parent feature %v",
			prerequisites,
			fc.Feature)
		for _, pre := range *prerequisites {
			prereqFeature := pre.Feature
			prereqFeatureConfig, err := e.query.GetFlag(prereqFeature)
			if err != nil {
				e.logger.Errorf(
					"Could not retrieve the pre requisite details of feature flag : %v", prereqFeature)
				return true, nil
			}

			prereqEvaluatedVariation, err := e.evaluateFlag(prereqFeatureConfig, target)
			if err != nil {
				e.logger.Errorf(
					"Could not evaluate the prerequisite details of feature flag : %v", prereqFeature)
				return true, nil
			}

			e.logger.Debugf(
				"Pre requisite flag %v has variation %v for target %v",
				prereqFeatureConfig.Feature,
				prereqEvaluatedVariation,
				target)

			// Compare if the pre requisite variation is a possible valid value of
			// the pre requisite FF
			validPrereqVariations := pre.Variations
			e.logger.Debugf(
				"Pre requisite flag %v should have the variations %v",
				prereqFeatureConfig.Feature,
				validPrereqVariations)
			if !contains(validPrereqVariations, prereqEvaluatedVariation.Identifier) {
				return false, nil
			}
			if r, _ := e.checkPreRequisite(&prereqFeatureConfig, target); !r {
				return false, nil
			}
		}
	}
	return true, nil
}

func (e Evaluator) evaluate(identifier string, target *Target, kind string) (rest.Variation, error) {

	if e.query == nil {
		e.logger.Errorf(ErrQueryProviderMissing.Error())
		return rest.Variation{}, ErrQueryProviderMissing
	}
	flag, err := e.query.GetFlag(identifier)
	if err != nil {
		return rest.Variation{}, err
	}
	if string(flag.Kind) != kind {
		return rest.Variation{}, fmt.Errorf("%w, expected: %s, got: %s", ErrFlagKindMismatch, kind, flag.Kind)
	}

	if flag.Prerequisites != nil {
		prereq, err := e.checkPreRequisite(&flag, target)
		if err != nil || !prereq {
			return findVariation(flag.Variations, flag.OffVariation)
		}
	}
	variation, err := e.evaluateFlag(flag, target)
	if err != nil {
		return rest.Variation{}, err
	}
	if e.postEvalCallback != nil {
		data := PostEvalData{
			FeatureConfig: &flag,
			Target:        target,
			Variation:     &variation,
		}

		e.postEvalCallback.PostEvaluateProcessor(&data)
	}
	return variation, nil
}

// BoolVariation returns boolean evaluation for target
func (e Evaluator) BoolVariation(identifier string, target *Target, defaultValue bool) bool {
	variation, err := e.evaluate(identifier, target, "boolean")
	if err != nil {
		e.logger.Errorf("Error while evaluating boolean flag '%s', err: %v", identifier, err)
		return defaultValue
	}
	return strings.ToLower(variation.Value) == "true"
}

// StringVariation returns string evaluation for target
func (e Evaluator) StringVariation(identifier string, target *Target, defaultValue string) string {

	variation, err := e.evaluate(identifier, target, "string")
	if err != nil {
		e.logger.Errorf("Error while evaluating string flag '%s', err: %v", identifier, err)
		return defaultValue
	}
	return variation.Value
}

// IntVariation returns int evaluation for target
func (e Evaluator) IntVariation(identifier string, target *Target, defaultValue int) int {

	variation, err := e.evaluate(identifier, target, "int")
	if err != nil {
		e.logger.Errorf("Error while evaluating int flag '%s', err: %v", identifier, err)
		return defaultValue
	}
	val, err := strconv.Atoi(variation.Value)
	if err != nil {
		return defaultValue
	}
	return val
}

// NumberVariation returns number evaluation for target
func (e Evaluator) NumberVariation(identifier string, target *Target, defaultValue float64) float64 {
	//all numbers are stored as ints in the database
	variation, err := e.evaluate(identifier, target, "int")
	if err != nil {
		e.logger.Errorf("Error while evaluating number flag '%s', err: %v", identifier, err)
		return defaultValue
	}
	val, err := strconv.ParseFloat(variation.Value, 64)
	if err != nil {
		return defaultValue
	}
	return val
}

// JSONVariation returns json evaluation for target
func (e Evaluator) JSONVariation(identifier string, target *Target,
	defaultValue map[string]interface{}) map[string]interface{} {

	variation, err := e.evaluate(identifier, target, "json")
	if err != nil {
		e.logger.Errorf("Error while evaluating json flag '%s', err: %v", identifier, err)
		return defaultValue
	}
	val := make(map[string]interface{})
	err = json.Unmarshal([]byte(variation.Value), &val)
	if err != nil {
		return defaultValue
	}
	return val
}
