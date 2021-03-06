/*
Copyright 2020 In Loco Tecnologia da Informação S.A.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wire

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	inlocov1alpha1 "github.com/inloco/kube-actions/operator/api/v1alpha1"
	"github.com/inloco/kube-actions/operator/controllers/actionsrunner/dot"
	"github.com/inloco/kube-actions/operator/controllers/actionsrunner/facades"
	"k8s.io/utils/strings"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type Wire struct {
	log    logr.Logger
	events chan<- event.GenericEvent

	ActionsRunner *inlocov1alpha1.ActionsRunner
	DotFiles      *dot.Files

	ghFacade  facades.GitHub
	adoFacade facades.AzureDevOps

	loopClose    chan struct{}
	loopAck      chan struct{}
	loopMessages chan Message

	gone bool
}

func (w *Wire) Init(ctx context.Context) error {
	if err := w.initDotFiles(); err != nil {
		return err
	}

	if err := w.ghFacade.Init(ctx, w.ActionsRunner.Spec.Repository.Owner, w.ActionsRunner.Spec.Repository.Name); err != nil {
		return err
	}
	w.DotFiles.Runner.GitHubUrl = w.ghFacade.Repository.GetGitCommitsURL()

	credential, err := w.ghFacade.GetGitHubTenantCredential(ctx, facades.RunnerEventRegister)
	if err != nil {
		return err
	}
	w.DotFiles.Runner.ServerUrl = credential.GetURL()

	if err := w.adoFacade.Init(ctx, credential.GetToken(), credential.GetURL(), w.DotFiles, w.ActionsRunner.Spec.Labels); err != nil {
		return err
	}

	if w.loopAck == nil {
		w.loopAck = make(chan struct{})
	}

	if w.loopMessages == nil {
		w.loopMessages = make(chan Message)
	}

	return nil
}

func (w *Wire) initDotFiles() error {
	if w.DotFiles != nil {
		return nil
	}

	dotRSAParameters, err := dot.NewRSAParameters()
	if err != nil {
		return err
	}

	w.DotFiles = &dot.Files{
		Runner: dot.Runner{
			AgentName:  strings.ShortenString(fmt.Sprintf("KA %s %s", w.ActionsRunner.GetNamespace(), w.ActionsRunner.GetName()), 64),
			PoolId:     1,
			PoolName:   "Default",
			WorkFolder: "_work",
		},
		Credentials: dot.Credentials{
			Scheme: "OAuth",
		},
		RSAParameters: *dotRSAParameters,
	}
	return nil
}

func (w *Wire) Channels(ctx context.Context) (<-chan struct{}, <-chan Message) {
	if !w.isClosed() {
		return w.loopAck, w.loopMessages
	}

	w.loopClose = make(chan struct{})
	w.log.Info("Wire Opened")

	go func() {
		genericEvent := event.GenericEvent{
			Meta: w.ActionsRunner,
		}

		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("%v", r)
				w.log.Error(err, err.Error())

				w.events <- genericEvent

				w.Close()
			}
		}()

		if err := w.adoFacade.InitAzureDevOpsTaskAgentSession(ctx); err != nil {
			w.gone = true
			w.log.Info("Wire Gone")

			panic(err)
		}
		defer w.adoFacade.DeinitAzureDevOpsTaskAgentSession(ctx)

		var lastMessageId *uint64
		for !w.isClosed() {
			w.log.Info("Getting Message")

			taMessage, err := w.adoFacade.GetMessage(ctx, lastMessageId)
			if err != nil {
				panic(err)
			}
			if taMessage == nil {
				continue
			}

			lastMessageId = taMessage.MessageId

			message, err := toMessage(*taMessage)
			if err != nil {
				panic(err)
			}

			w.log.Info("Message Received", "id", message.Id, "type", message.Type)

			if err := w.adoFacade.DeinitAzureDevOpsTaskAgentSession(ctx); err != nil {
				panic(err)
			}

			w.events <- genericEvent
			w.loopMessages <- *message
			w.loopAck <- struct{}{}

			if w.isClosed() {
				break
			}

			if err := w.adoFacade.InitAzureDevOpsTaskAgentSession(ctx); err != nil {
				panic(err)
			}

			if message.Type != MessageTypePipelineAgentJobRequest {
				w.log.Info("Deleting Message", "id", message.Id, "type", message.Type)

				if err := w.adoFacade.DeleteMessage(ctx, *taMessage); err != nil {
					panic(err)
				}

				w.log.Info("Message Deleted", "id", message.Id, "type", message.Type)
			}

			if message.Type == MessageTypeAgentRefresh {
				w.log.Info("Deleting Agent", "id", message.Id, "type", message.Type)

				if err := w.adoFacade.DeleteAgent(ctx); err != nil {
					panic(err)
				}

				w.log.Info("Agent Deleted", "id", message.Id, "type", message.Type)

				break
			}
		}
	}()

	return w.loopAck, w.loopMessages
}

func (w *Wire) Close() error {
	if w.isClosed() {
		return errors.New(".isClosed")
	}

	close(w.loopClose)
	w.adoFacade.DeinitAzureDevOpsTaskAgentSession(context.Background())

	w.log.Info("Wire Closed")
	return nil
}

func (w *Wire) isClosed() bool {
	select {
	case _, ok := <-w.loopClose:
		return !ok
	default:
		return w.loopClose == nil
	}
}
