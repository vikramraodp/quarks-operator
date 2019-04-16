package manifest

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	btg "github.com/viovanov/bosh-template-go"
	"go.uber.org/zap"
	yaml "gopkg.in/yaml.v2"

	"code.cloudfoundry.org/cf-operator/pkg/bosh/bpm"
)

// DataGatherer gathers data for jobs in the manifest, it handles links and returns a resolved manifest
type DataGatherer struct {
	log      *zap.SugaredLogger
	manifest *Manifest
}

// NewDataGatherer returns a data gatherer with logging for a given input manifest
func NewDataGatherer(log *zap.SugaredLogger, manifest *Manifest) *DataGatherer {
	return &DataGatherer{
		log:      log,
		manifest: manifest,
	}
}

// GatherData will collect different data
// Collect job spec information
// Collect job properties
// Collect bosh links
// Render the bpm yaml file data
func (dg *DataGatherer) GatherData(baseDir string, cfOperatorNamespace string, instanceGroupName string) ([]byte, error) {
	jobReleaseSpecs, jobProviderLinks, err := dg.CollectReleaseSpecsAndProviderLinks(baseDir, cfOperatorNamespace)
	if err != nil {
		return nil, err
	}

	return dg.ProcessConsumersAndRenderBPM(baseDir, jobReleaseSpecs, jobProviderLinks, instanceGroupName)
}

// CollectReleaseSpecsAndProviderLinks will collect all release specs and bosh links for provider jobs
func (dg *DataGatherer) CollectReleaseSpecsAndProviderLinks(baseDir string, cfOperatorNamespace string) (map[string]map[string]JobSpec, map[string]map[string]JobLink, error) {
	// Contains YAML.load('.../release_name/job_name/job.MF')
	jobReleaseSpecs := map[string]map[string]JobSpec{}

	// Lists every link provided by the job
	jobProviderLinks := map[string]map[string]JobLink{}

	for _, instanceGroup := range dg.manifest.InstanceGroups {
		for jobIdx, job := range instanceGroup.Jobs {
			// make sure a map entry exists for the current job release
			if _, ok := jobReleaseSpecs[job.Release]; !ok {
				jobReleaseSpecs[job.Release] = map[string]JobSpec{}
			}

			// load job.MF into jobReleaseSpecs[job.Release][job.Name]
			if _, ok := jobReleaseSpecs[job.Release][job.Name]; !ok {
				jobMFFilePath := filepath.Join(baseDir, "jobs-src", job.Release, job.Name, "job.MF")
				jobMfBytes, err := ioutil.ReadFile(jobMFFilePath)
				if err != nil {
					return nil, nil, err
				}

				jobSpec := JobSpec{}
				if err := yaml.Unmarshal([]byte(jobMfBytes), &jobSpec); err != nil {
					return nil, nil, err
				}
				jobReleaseSpecs[job.Release][job.Name] = jobSpec
			}

			// spec of the current jobs release/name
			spec := jobReleaseSpecs[job.Release][job.Name]

			// Generate instance spec for each ig instance
			// This will be stored inside the current job under
			// job.properties.bosh_containerization
			var jobsInstances []JobInstance
			for i := 0; i < instanceGroup.Instances; i++ {

				// TODO: Understand whether there are negative side-effects to using this
				// default
				azs := []string{""}
				if len(instanceGroup.Azs) > 0 {
					azs = instanceGroup.Azs
				}

				for _, az := range azs {
					index := len(jobsInstances)
					name := fmt.Sprintf("%s-%s", instanceGroup.Name, job.Name)
					id := fmt.Sprintf("%v-%v-%v", instanceGroup.Name, index, job.Name)
					// TODO: not allowed to hardcode svc.cluster.local
					address := fmt.Sprintf("%s.%s.svc.cluster.local", id, cfOperatorNamespace)

					jobsInstances = append(jobsInstances, JobInstance{
						Address:  address,
						AZ:       az,
						ID:       id,
						Index:    index,
						Instance: i,
						Name:     name,
					})
				}
			}

			// set jobs.properties.bosh_containerization.instances with the ig instances
			instanceGroup.Jobs[jobIdx].Properties.BOSHContainerization.Instances = jobsInstances

			// Create a list of fully evaluated links provided by the current job
			// These is specified in the job release job.MF file
			if spec.Provides != nil {
				var properties map[string]interface{}

				for _, provider := range spec.Provides {
					properties = map[string]interface{}{}
					for _, property := range provider.Properties {
						// generate a nested struct of map[string]interface{} when
						// a property is of the form foo.bar
						if strings.Contains(property, ".") {
							propertyStruct := spec.RetrieveNestedProperty(property)
							properties = propertyStruct
						} else {
							properties[property] = spec.RetrievePropertyDefault(property)
						}
					}
					// Override default spec values with explicit settings from the
					// current bosh deployment manifest, this should be done under each
					// job, inside a `properties` key.
					for propertyName := range properties {
						if explicitSetting, ok := job.Property(propertyName); ok {
							properties[propertyName] = explicitSetting
						}
					}
					providerName := provider.Name
					providerType := provider.Type

					// instance_group.job can override the link name through the
					// instance_group.job.provides, via the "as" key
					if instanceGroup.Jobs[jobIdx].Provides != nil {
						if value, ok := instanceGroup.Jobs[jobIdx].Provides[providerName]; ok {
							switch value.(type) {
							case map[interface{}]interface{}:
								if overrideLinkName, ok := value.(map[interface{}]interface{})["as"]; ok {
									providerName = fmt.Sprintf("%v", overrideLinkName)
								}
							default:
								return nil, nil, fmt.Errorf("unexpected type detected: %T, should have been a map", value)
							}

						}
					}

					if providers, ok := jobProviderLinks[providerType]; ok {
						if _, ok := providers[providerName]; ok {
							return nil, nil, fmt.Errorf("multiple providers for link: name=%s type=%s", providerName, providerType)
						}
					}

					if _, ok := jobProviderLinks[providerType]; !ok {
						jobProviderLinks[providerType] = map[string]JobLink{}
					}

					// construct the jobProviderLinks of the current job that provides
					// a link
					jobProviderLinks[providerType][providerName] = JobLink{
						Instances:  jobsInstances,
						Properties: properties,
					}
				}
			}
		}
	}

	return jobReleaseSpecs, jobProviderLinks, nil
}

// generateJobConsumersData will populate a job with its corresponding provider links
// under properties.bosh_containerization.consumes
func generateJobConsumersData(currentJob *Job, jobReleaseSpecs map[string]map[string]JobSpec, jobProviderLinks map[string]map[string]JobLink) error {
	currentJobSpecData := jobReleaseSpecs[currentJob.Release][currentJob.Name]
	for _, consumes := range currentJobSpecData.Consumes {

		consumesName := consumes.Name

		if currentJob.Consumes != nil {
			// Deployment manifest can intentionally prevent link resolution as long as the link is optional
			// Continue to the next job if this one does not consumes links
			if _, ok := currentJob.Consumes[consumesName]; !ok {
				if consumes.Optional {
					continue
				}
				return fmt.Errorf("mandatory link of consumer %s is explicitly set to nil", consumesName)
			}

			// When the job defines a consumes property in the manifest, use it instead of the one
			// from currentJobSpecData.Consumes
			if _, ok := currentJob.Consumes[consumesName]; ok {
				if value, ok := currentJob.Consumes[consumesName].(map[interface{}]interface{})["from"]; ok {
					consumesName = value.(string)
				}
			}
		}

		link, hasLink := jobProviderLinks[consumes.Type][consumesName]
		if !hasLink && !consumes.Optional {
			return fmt.Errorf("cannot resolve non-optional link for consumer %s", consumesName)
		}

		// generate the job.properties.bosh_containerization.consumes struct with the links information from providers.
		if currentJob.Properties.BOSHContainerization.Consumes == nil {
			currentJob.Properties.BOSHContainerization.Consumes = map[string]JobLink{}
		}

		currentJob.Properties.BOSHContainerization.Consumes[consumesName] = JobLink{
			Instances:  link.Instances,
			Properties: link.Properties,
		}
	}
	return nil
}

// renderJobBPM per job and add its value to the jobInstances.BPM field
func (dg *DataGatherer) renderJobBPM(currentJob *Job, jobInstances []JobInstance, baseDir string, manifestName string) error {

	// Location of the current job job.MF file
	jobSpecFile := filepath.Join(baseDir, "jobs-src", currentJob.Release, currentJob.Name, "job.MF")

	var jobSpec struct {
		Templates map[string]string `yaml:"templates"`
	}

	// First, we must figure out the location of the template.
	// We're looking for a template in the spec, whose result is a file "bpm.yml"
	yamlFile, err := ioutil.ReadFile(jobSpecFile)
	if err != nil {
		return errors.Wrap(err, "failed to read the job spec file")
	}
	err = yaml.Unmarshal(yamlFile, &jobSpec)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal the job spec file")
	}

	bpmSource := ""
	for srcFile, dstFile := range jobSpec.Templates {
		if filepath.Base(dstFile) == "bpm.yml" {
			bpmSource = srcFile
			break
		}
	}

	if bpmSource == "" {
		return fmt.Errorf("can't find BPM template for job %s", currentJob.Name)
	}

	// ### Render bpm.yml.erb for each job instance

	erbFilePath := filepath.Join(baseDir, "jobs-src", currentJob.Release, currentJob.Name, "templates", bpmSource)
	if _, err := os.Stat(erbFilePath); err != nil {
		return err
	}

	jobIndexBPM := make([]bpm.Config, len(jobInstances))

	if jobInstances != nil {
		for i, jobInstance := range jobInstances {

			properties := currentJob.Properties.ToMap()

			renderPointer := btg.NewERBRenderer(
				&btg.EvaluationContext{
					Properties: properties,
				},

				&btg.InstanceInfo{
					Address:    jobInstance.Address,
					AZ:         jobInstance.AZ,
					ID:         jobInstance.ID,
					Index:      string(jobInstance.Index),
					Deployment: manifestName,
					Name:       jobInstance.Name,
				},

				jobSpecFile,
			)

			// Write to a tmp, this is following the conventions on how the
			// https://github.com/viovanov/bosh-template-go/ processes the params
			// when we calling the *.Render()
			tmpfile, err := ioutil.TempFile("", "rendered.*.yml")
			if err != nil {
				return err
			}
			defer os.Remove(tmpfile.Name())

			if err := renderPointer.Render(erbFilePath, tmpfile.Name()); err != nil {
				return err
			}

			bpmBytes, err := ioutil.ReadFile(tmpfile.Name())
			if err != nil {
				return err
			}

			// Parse a rendered bpm.yml into the bpm Config struct
			jobIndexBPM[i], err = bpm.NewConfig(bpmBytes)
			if err != nil {
				return err
			}
		}

		for _, jobBPMInstance := range jobIndexBPM {
			if !reflect.DeepEqual(jobBPMInstance, jobIndexBPM[0]) {
				dg.log.Warnf("found different BPM job indexes for job %s in manifest %s, this is NOT SUPPORTED", currentJob.Name, manifestName)
			}
		}
		currentJob.Properties.BOSHContainerization.BPM = jobIndexBPM[0]
	}
	return nil
}

// ProcessConsumersAndRenderBPM will generate a proper context for links and render the required ERB files
func (dg *DataGatherer) ProcessConsumersAndRenderBPM(baseDir string, jobReleaseSpecs map[string]map[string]JobSpec, jobProviderLinks map[string]map[string]JobLink, instanceGroupName string) ([]byte, error) {
	var desiredInstanceGroup *InstanceGroup
	for _, instanceGroup := range dg.manifest.InstanceGroups {
		if instanceGroup.Name != instanceGroupName {
			continue
		}

		desiredInstanceGroup = instanceGroup
		break
	}

	if desiredInstanceGroup == nil {
		return nil, errors.Errorf("can't find instance group '%s' in manifest", instanceGroupName)
	}

	for idJob, job := range desiredInstanceGroup.Jobs {

		currentJob := &desiredInstanceGroup.Jobs[idJob]

		// Verify that the current job release exists on the manifest releases block
		if lookUpJobRelease(dg.manifest.Releases, job.Release) {
			currentJob.Properties.BOSHContainerization.Release = job.Release
		}

		err := generateJobConsumersData(currentJob, jobReleaseSpecs, jobProviderLinks)
		if err != nil {
			return nil, err
		}

		// Get current job.bosh_containerization.instances, which will be required by the renderer to generate
		// the render.InstanceInfo struct
		jobInstances := currentJob.Properties.BOSHContainerization.Instances

		err = dg.renderJobBPM(currentJob, jobInstances, baseDir, dg.manifest.Name)
		if err != nil {
			return nil, err
		}

		// Store shared bpm as a top level property
		if len(jobInstances) < 1 {
			continue
		}
	}

	// marshall the whole manifest Structure
	manifestResolved, err := yaml.Marshal(dg.manifest)
	if err != nil {
		return nil, err
	}

	return manifestResolved, nil
}

// Property search for property value in the job properties
func (job Job) Property(propertyName string) (interface{}, bool) {
	var pointer interface{}

	pointer = job.Properties.Properties
	for _, pathPart := range strings.Split(propertyName, ".") {
		switch pointer.(type) {
		case map[string]interface{}:
			hash := pointer.(map[string]interface{})
			if _, ok := hash[pathPart]; !ok {
				return nil, false
			}
			pointer = hash[pathPart]

		case map[interface{}]interface{}:
			hash := pointer.(map[interface{}]interface{})
			if _, ok := hash[pathPart]; !ok {
				return nil, false
			}
			pointer = hash[pathPart]

		default:
			return nil, false
		}
	}
	return pointer, true
}

// RetrieveNestedProperty will generate an nested struct
// based on a string of the type foo.bar
func (js JobSpec) RetrieveNestedProperty(propertyName string) map[string]interface{} {
	var anStruct map[string]interface{}
	var previous map[string]interface{}
	items := strings.Split(propertyName, ".")
	for i := len(items) - 1; i >= 0; i-- {
		if i == (len(items) - 1) {
			previous = map[string]interface{}{
				items[i]: js.RetrievePropertyDefault(propertyName),
			}
		} else {
			anStruct = map[string]interface{}{
				items[i]: previous,
			}
			previous = anStruct

		}
	}
	return anStruct
}

// RetrievePropertyDefault return the default value of the spec property
func (js JobSpec) RetrievePropertyDefault(propertyName string) interface{} {
	if property, ok := js.Properties[propertyName]; ok {
		return property.Default
	}

	return nil
}

// lookUpJobRelease will check in the main manifest for
// a release name
func lookUpJobRelease(releases []*Release, jobRelease string) bool {
	for _, release := range releases {
		if release.Name == jobRelease {
			return true
		}
	}

	return false
}
