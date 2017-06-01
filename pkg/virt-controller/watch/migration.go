package watch

import (
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	kubeapi "k8s.io/client-go/pkg/api"
	k8sv1 "k8s.io/client-go/pkg/api/v1"
	metav1 "k8s.io/client-go/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/labels"
	"k8s.io/client-go/pkg/runtime"
	"k8s.io/client-go/pkg/util/wait"
	"k8s.io/client-go/pkg/util/workqueue"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	kubev1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/logging"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

func NewMigrationController(migrationService services.VMService, restClient *rest.RESTClient, clientset *kubernetes.Clientset) *MigrationController {
	lw := cache.NewListWatchFromClient(restClient, "migrations", k8sv1.NamespaceDefault, fields.Everything())
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	store, informer := cache.NewIndexerInformer(lw, &kubev1.Migration{}, 0, kubecli.NewResourceEventHandlerFuncsForWorkqueue(queue), cache.Indexers{})
	return &MigrationController{
		restClient: restClient,
		vmService:  migrationService,
		clientset:  clientset,
		queue:      queue,
		store:      store,
		informer:   informer,
	}
}

type MigrationController struct {
	restClient *rest.RESTClient
	vmService  services.VMService
	clientset  *kubernetes.Clientset
	queue      workqueue.RateLimitingInterface
	store      cache.Store
	informer   cache.ControllerInterface
}

func (c *MigrationController) Run(threadiness int, stopCh chan struct{}) {
	defer kubecli.HandlePanic()
	defer c.queue.ShutDown()
	logging.DefaultLogger().Info().Msg("Starting controller.")

	// Start all informers and wait for the cache sync
	_, jobInformer := NewMigrationJobInformer(c.clientset, c.queue)
	go jobInformer.Run(stopCh)
	_, podInformer := NewMigrationPodInformer(c.clientset, c.queue)
	go podInformer.Run(stopCh)
	go c.informer.Run(stopCh)
	cache.WaitForCacheSync(stopCh, c.informer.HasSynced, jobInformer.HasSynced, podInformer.HasSynced)

	// Start the actual work
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	logging.DefaultLogger().Info().Msg("Stopping controller.")
}

func (c *MigrationController) runWorker() {
	for c.Execute() {
	}
}

func (md *MigrationController) Execute() bool {
	key, quit := md.queue.Get()
	if quit {
		return false
	}
	defer md.queue.Done(key)
	if err := md.execute(key.(string)); err != nil {
		logging.DefaultLogger().Info().Reason(err).Msgf("reenqueuing migration %v", key)
		md.queue.AddRateLimited(key)
	} else {
		logging.DefaultLogger().Info().V(4).Msgf("processed migration %v", key)
		md.queue.Forget(key)
	}
	return true
}

func (md *MigrationController) execute(key string) error {

	setMigrationPhase := func(migration *kubev1.Migration, phase kubev1.MigrationPhase) error {

		if migration.Status.Phase == phase {
			return nil
		}

		logger := logging.DefaultLogger().Object(migration)

		migration.Status.Phase = phase
		// TODO indicate why it was set to failed
		err := md.vmService.UpdateMigration(migration)
		if err != nil {
			logger.Error().Reason(err).Msgf("updating migration state failed: %v ", err)
			return err
		}
		return nil
	}

	setMigrationFailed := func(mig *kubev1.Migration) error {
		return setMigrationPhase(mig, kubev1.MigrationFailed)
	}

	obj, exists, err := md.store.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	logger := logging.DefaultLogger().Object(obj.(*kubev1.Migration))
	// Copy migration for future modifications
	if obj, err = kubeapi.Scheme.Copy(obj.(runtime.Object)); err != nil {
		logger.Error().Reason(err).Msg("could not copy migration object")
		return err
	}
	migration := obj.(*kubev1.Migration)

	vm, exists, err := md.vmService.FetchVM(migration.Spec.Selector.Name)
	if err != nil {
		logger.Error().Reason(err).Msgf("fetching the vm %s failed", migration.Spec.Selector.Name)
		return err
	}

	if !exists {
		logger.Info().Msgf("VM with name %s does not exist, marking migration as failed", migration.Spec.Selector.Name)
		return setMigrationFailed(migration)
	}

	switch migration.Status.Phase {
	case kubev1.MigrationUnknown:
		if vm.Status.Phase != kubev1.Running {
			logger.Error().Msgf("VM with name %s is in state %s, no migration possible. Marking migration as failed", vm.GetObjectMeta().GetName(), vm.Status.Phase)
			return setMigrationFailed(migration)
		}

		if err := mergeConstraints(migration, vm); err != nil {
			logger.Error().Reason(err).Msg("merging Migration and VM placement constraints failed.")
			return err
		}
		podList, err := md.vmService.GetRunningVMPods(vm)
		if err != nil {
			logger.Error().Reason(err).Msg("could not fetch a list of running VM target pods")
			return err
		}

		//FIXME when we have more than one worker, we need a lock on the VM
		numOfPods, targetPod, err := investigateTargetPodSituation(migration, podList, md.store)
		if err != nil {
			logger.Error().Reason(err).Msg("could not investigate pods")
			return err
		}

		if targetPod == nil {
			if numOfPods >= 1 {
				logger.Error().Msg("another migration seems to be in progress, marking Migration as failed")
				// Another migration is currently going on
				if err = setMigrationFailed(migration); err != nil {
					return err
				}
				return nil
			} else if numOfPods == 0 {
				// We need to start a migration target pod
				// TODO, this detection is not optimal, it can lead to strange situations
				err := md.vmService.CreateMigrationTargetPod(migration, vm)
				if err != nil {
					logger.Error().Reason(err).Msg("creating a migration target pod failed")
					return err
				}
			}
		} else {
			if targetPod.Status.Phase == k8sv1.PodFailed {
				logger.Error().Msg("migration target pod is in failed state")
				return setMigrationFailed(migration)
			}
			// Unlikely to hit this case, but prevents erroring out
			// if we re-enter this loop
			logger.Info().Msgf("migration appears to be set up, but was not set to %s", kubev1.MigrationRunning)
		}
		return setMigrationPhase(migration, kubev1.MigrationRunning)
	case kubev1.MigrationRunning:
		podList, err := md.vmService.GetRunningVMPods(vm)
		if err != nil {
			logger.Error().Reason(err).Msg("could not fetch a list of running VM target pods")
			return err
		}
		_, targetPod, err := investigateTargetPodSituation(migration, podList, md.store)
		if err != nil {
			logger.Error().Reason(err).Msg("could not investigate pods")
			return err
		}
		if targetPod == nil {
			logger.Error().Msg("migration target pod does not exist or is in an end state")
			return setMigrationFailed(migration)
		}
		switch targetPod.Status.Phase {
		case k8sv1.PodRunning:
			break
		case k8sv1.PodSucceeded, k8sv1.PodFailed:
			logger.Error().Msgf("migration target pod is in end state %s", targetPod.Status.Phase)
			return setMigrationFailed(migration)
		default:
			//Not requeuing, just not far enough along to proceed
			logger.Info().V(3).Msg("target Pod not running yet")
			return nil
		}

		if vm.Status.MigrationNodeName != targetPod.Spec.NodeName {
			vm.Status.Phase = kubev1.Migrating
			vm.Status.MigrationNodeName = targetPod.Spec.NodeName
			if _, err = md.vmService.PutVm(vm); err != nil {
				logger.Error().Reason(err).Msgf("failed to update VM to state %s", kubev1.Migrating)
				return err
			}
		}

		// Let's check if the job already exists, it can already exist in case we could not update the VM object in a previous run
		migrationPod, exists, err := md.vmService.GetMigrationJob(migration)

		if err != nil {
			logger.Error().Reason(err).Msg("Checking for an existing migration job failed.")
			return err
		}

		if !exists {
			sourceNode, err := md.clientset.CoreV1().Nodes().Get(vm.Status.NodeName, metav1.GetOptions{})
			if err != nil {
				logger.Error().Reason(err).Msgf("fetching source node %s failed", vm.Status.NodeName)
				return err
			}
			targetNode, err := md.clientset.CoreV1().Nodes().Get(vm.Status.MigrationNodeName, metav1.GetOptions{})
			if err != nil {
				logger.Error().Reason(err).Msgf("fetching target node %s failed", vm.Status.MigrationNodeName)
				return err
			}

			if err := md.vmService.StartMigration(migration, vm, sourceNode, targetNode, targetPod); err != nil {
				logger.Error().Reason(err).Msg("Starting the migration job failed.")
				return err
			}
			return nil
		}

		// FIXME, the final state updates must come from virt-handler
		switch migrationPod.Status.Phase {
		case k8sv1.PodFailed:
			vm.Status.Phase = kubev1.Running
			vm.Status.MigrationNodeName = ""
			if _, err = md.vmService.PutVm(vm); err != nil {
				return err
			}
			return setMigrationFailed(migration)
		case k8sv1.PodSucceeded:
			vm.Status.NodeName = targetPod.Spec.NodeName
			vm.Status.MigrationNodeName = ""
			vm.Status.Phase = kubev1.Running
			if vm.ObjectMeta.Labels == nil {
				vm.ObjectMeta.Labels = map[string]string{}
			}
			vm.ObjectMeta.Labels[kubev1.NodeNameLabel] = vm.Status.NodeName
			if _, err = md.vmService.PutVm(vm); err != nil {
				logger.Error().Reason(err).Msg("updating the VM failed.")
				return err
			}
			return setMigrationPhase(migration, kubev1.MigrationSucceeded)
		}
	}
	return nil
}

// Returns the number of  running pods and if a pod for exactly that migration is currently running
func investigateTargetPodSituation(migration *kubev1.Migration, podList *k8sv1.PodList, migrationStore cache.Store) (int, *k8sv1.Pod, error) {
	var targetPod *k8sv1.Pod = nil
	podCount := 0
	for idx, pod := range podList.Items {
		if pod.Labels[kubev1.MigrationUIDLabel] == string(migration.GetObjectMeta().GetUID()) {
			targetPod = &podList.Items[idx]
			podCount += 1
			continue
		}

		// The first pod was never part of a migration, it does not count
		l, exists := pod.Labels[kubev1.MigrationLabel]
		if !exists {
			continue
		}
		key := fmt.Sprintf("%s/%s", pod.ObjectMeta.Namespace, l)
		cachedObj, exists, err := migrationStore.GetByKey(key)
		if err != nil {
			return 0, nil, err
		}
		if exists {
			cachedMigration := cachedObj.(*kubev1.Migration)
			if (cachedMigration.Status.Phase != kubev1.MigrationFailed) &&
				(cachedMigration.Status.Phase) != kubev1.MigrationSucceeded {
				podCount += 1
			}
		} else {
			podCount += 1
		}
	}
	return podCount, targetPod, nil
}

func mergeConstraints(migration *kubev1.Migration, vm *kubev1.VM) error {

	merged := map[string]string{}
	for k, v := range vm.Spec.NodeSelector {
		merged[k] = v
	}
	conflicts := []string{}
	for k, v := range migration.Spec.NodeSelector {
		val, exists := vm.Spec.NodeSelector[k]
		if exists && val != v {
			conflicts = append(conflicts, k)
		} else {
			merged[k] = v
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("Conflicting node selectors: %v", conflicts)
	}
	vm.Spec.NodeSelector = merged
	return nil
}

func migrationVMPodSelector() kubeapi.ListOptions {
	fieldSelectionQuery := fmt.Sprintf("status.phase=%s", string(kubeapi.PodRunning))
	fieldSelector := fields.ParseSelectorOrDie(fieldSelectionQuery)
	labelSelectorQuery := fmt.Sprintf("%s, %s in (virt-launcher)", string(kubev1.MigrationLabel), kubev1.AppLabel)
	labelSelector, err := labels.Parse(labelSelectorQuery)

	if err != nil {
		panic(err)
	}
	return kubeapi.ListOptions{FieldSelector: fieldSelector, LabelSelector: labelSelector}
}

func migrationJobSelector() kubeapi.ListOptions {
	fieldSelector := fields.ParseSelectorOrDie(
		"status.phase!=" + string(k8sv1.PodPending) +
			",status.phase!=" + string(k8sv1.PodRunning) +
			",status.phase!=" + string(k8sv1.PodUnknown))
	labelSelector, err := labels.Parse(kubev1.AppLabel + "=migration," + kubev1.DomainLabel + "," + kubev1.MigrationLabel)
	if err != nil {
		panic(err)
	}
	return kubeapi.ListOptions{FieldSelector: fieldSelector, LabelSelector: labelSelector}
}

// Informer, which checks for Jobs, orchestrating the migrations done by libvirt
func NewMigrationJobInformer(clientSet *kubernetes.Clientset, migrationQueue workqueue.RateLimitingInterface) (cache.Store, cache.ControllerInterface) {
	selector := migrationJobSelector()
	lw := kubecli.NewListWatchFromClient(clientSet.CoreV1().RESTClient(), "pods", kubeapi.NamespaceDefault, selector.FieldSelector, selector.LabelSelector)
	return cache.NewIndexerInformer(lw, &k8sv1.Pod{}, 0,
		kubecli.NewResourceEventHandlerFuncsForFunc(migrationLabelHandler(migrationQueue)),
		cache.Indexers{})
}

// Informer, which checks for potential migration target Pods
func NewMigrationPodInformer(clientSet *kubernetes.Clientset, migrationQueue workqueue.RateLimitingInterface) (cache.Store, cache.ControllerInterface) {
	selector := migrationVMPodSelector()
	lw := kubecli.NewListWatchFromClient(clientSet.CoreV1().RESTClient(), "pods", kubeapi.NamespaceDefault, selector.FieldSelector, selector.LabelSelector)
	return cache.NewIndexerInformer(lw, &k8sv1.Pod{}, 0,
		kubecli.NewResourceEventHandlerFuncsForFunc(migrationLabelHandler(migrationQueue)),
		cache.Indexers{})
}

func migrationLabelHandler(migrationQueue workqueue.RateLimitingInterface) func(obj interface{}) {
	return func(obj interface{}) {
		migrationLabel := obj.(*k8sv1.Pod).ObjectMeta.Labels[kubev1.MigrationLabel]
		migrationQueue.Add(k8sv1.NamespaceDefault + "/" + migrationLabel)
	}
}